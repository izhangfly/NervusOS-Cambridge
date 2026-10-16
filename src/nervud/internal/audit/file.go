// This file is the append-only audit file recorder: one JSON object per line, linked into a hash chain (chain.go).
//
// # Why audit can't just write to slog
//
// Three reasons, each of which can make audit unavailable at exactly the moment it's needed:
//
//  1. The log level can filter it out. Audit isn't debug information.
//
//  2. Mixed in with ordinary logs, it gets overwritten by the rotation policy under load, and what gets overwritten is usually the earliest part,
//
//     the start of the incident.
//
//  3. Nothing stops later writes from overwriting it, and nothing can prove it was never overwritten.
//
// But slog is still written. Each path does its own job: the file is for verifiability and retention, slog is for
// "ops can see it at a glance with journalctl". Dropping the latter would make troubleshooting harder, and that's what audit is used for day to day.
//
// # What happens when a write fails
//
// Audit is a security control, and dropping records silently makes it useless; but stopping the robot because the disk is full is just as
// unacceptable. The compromise is to make the gap visible: count failed writes, and once writes work again immediately record an
// audit.ChainGap saying how many were lost. The chain then shows a sequence jump, which is exactly what we want:
// to the verifier it's the same kind of anomaly as "someone deleted records", and a person must see both.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultDir is the production image's audit directory. preflight creates it with mode 0700.
	DefaultDir = "/var/lib/nervus/audit"

	// FileName is the current audit file name. Rotated history files get .1, .2 suffixes.
	FileName = "audit.jsonl"

	// filePerm is 0600: audit contains package ids, uids and refusal reasons.
	// No bits for group or other: audit's readers are operators, not the Apps on the machine.
	filePerm os.FileMode = 0o600
	dirPerm  os.FileMode = 0o700

	// ActionChainStarted marks the start of a new chain.
	//
	// It must be recorded explicitly: without it, "the process restarted" and "someone deleted the file and recreated it"
	// look exactly the same on the chain, both starting at seq 1.
	ActionChainStarted = "audit.ChainStarted"

	// ActionChainGap marks a run of records lost because writes failed.
	ActionChainGap = "audit.ChainGap"

	// ActionRotated marks a rotation. It carries the last hash of the previous file,
	// so the chain still links across files; otherwise every rotation would look like a truncation.
	ActionRotated = "audit.Rotated"

	// ActionClosed is the final record of a clean shutdown; the file is synced and closed only after it's written.
	//
	// Its value isn't in itself but in its absence: if the next start finds the chain doesn't end with it, it knows the last run
	// ended in a power cut or KILL, and up to minSyncInterval of records may be missing at that point.
	ActionClosed = "audit.Closed"

	// ActionUncleanShutdown is recorded at startup, marking the end of the previous run as untrustworthy.
	ActionUncleanShutdown = "audit.UncleanShutdown"
)

// minSyncInterval is the minimum interval between two fsyncs, and also the upper bound of the power-cut loss window.
//
// # Why not sync every record
//
// Writes and fsyncs both happen on the write goroutine, and callers return as soon as they enqueue, so fsync doesn't slow down any
// business path. But it does slow the write goroutine itself: one fsync on an SD card takes a few to tens of milliseconds,
// so syncing every record would cut the drain rate to a few dozen a second; in a burst the queue backs up, overflows and loses records.
// Trying not to lose records would lose them, which is exactly backwards.
//
// # Why 1 second
//
// With batching, one fsync covers every record in that second, spreading write amplification across them. The cost is that a power cut can lose
// up to 1 second, and that second is marked by the UncleanShutdown at the next start: the window is known,
// not invisible. It's the same trade-off as journald's SyncIntervalSec.
const minSyncInterval = time.Second

// FileConfig holds the file recorder's construction parameters.
type FileConfig struct {
	// Dir is the audit directory, such as /var/lib/nervus/audit.
	Dir string

	// MaxBytes is the rotation threshold for one file. <= 0 means DefaultMaxBytes.
	MaxBytes int64

	// MaxFiles is the number of history files kept (excluding the current one). <= 0 means DefaultMaxFiles.
	MaxFiles int

	// QueueDepth is the depth of the async queue. <= 0 means DefaultQueueDepth.
	QueueDepth int

	// Log is the mirror output, also used to report the recorder's own faults. Required.
	Log *slog.Logger

	// now is for test injection. Always time.Now in production.
	now func() time.Time
}

const (
	// DefaultMaxBytes is 16 MiB: on a robot, audit events are rare, so this size lasts a long time,
	// while very large files would make the verifier struggle to read one into memory.
	DefaultMaxBytes = 16 << 20
	// DefaultMaxFiles keeps 4 history files; with the current one that's a ceiling of 5 x 16 MiB = 80 MiB.
	DefaultMaxFiles = 4
	// DefaultQueueDepth is the same order of magnitude as the log's 512: enough to absorb bursts without
	// piling up too many unsynced security events in the queue.
	DefaultQueueDepth = 512
)

// FileRecorder writes audit as an append-only hash-chained file.
type FileRecorder struct {
	dir      string
	maxBytes int64
	maxFiles int
	log      *slog.Logger
	now      func() time.Time

	queue chan Event
	done  chan struct{}
	stop  chan struct{}
	once  sync.Once

	// dropped is the running count of failed enqueues (queue full); the write goroutine turns it into a ChainGap at the next
	// successful write.
	dropped atomic.Uint64

	// written / synced are the running counts of records written and fsyncs.
	//
	// Their ratio shows how well batching works: 1000 records with only 20 fsyncs means the throttle is
	// working; if the two are close, nearly every record was Denied, and it's worth looking at what keeps being refused.
	// It's logged at shutdown, and also lets tests assert timing without touching the write goroutine's private state.
	written atomic.Uint64
	synced  atomic.Uint64

	// Only the write goroutine touches the fields below; no lock.
	file    *os.File
	size    int64
	seq     uint64
	prev    string
	pending uint64 // records known to be lost but not yet written into a ChainGap

	// dirty means there are written but not yet fsynced records.
	//
	// It means no fsync at all happens while idle. Without this check, the timer would wake the storage device
	// unconditionally every second. A robot produces no audit events most of the time, so that would be pure SD card wear
	// and power draw, buying no durability at all.
	dirty    bool
	lastSync time.Time
}

// NewFileRecorder opens (or creates) the audit file and starts the write goroutine.
//
// Failing to open is a hard error: a system that runs but records no audit is worse than one that won't start,
// because the former makes people think there's an audit. The caller should let it bubble up into a startup failure.
func NewFileRecorder(cfg FileConfig) (*FileRecorder, error) {
	if cfg.Log == nil {
		return nil, errors.New("audit: FileConfig.Log is required")
	}
	if cfg.Dir == "" {
		return nil, errors.New("audit: FileConfig.Dir is required")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = DefaultMaxFiles
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = DefaultQueueDepth
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}

	if err := os.MkdirAll(cfg.Dir, dirPerm); err != nil {
		return nil, fmt.Errorf("audit: create %s: %w", cfg.Dir, err)
	}

	r := &FileRecorder{
		dir:      cfg.Dir,
		maxBytes: cfg.MaxBytes,
		maxFiles: cfg.MaxFiles,
		log:      cfg.Log,
		now:      cfg.now,
		queue:    make(chan Event, cfg.QueueDepth),
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	go r.run()
	return r, nil
}

// open opens the current file and recovers the chain tail.
func (r *FileRecorder) open() error {
	path := filepath.Join(r.dir, FileName)
	// O_APPEND: every write is atomically appended to the end, so writes can't overwrite each other even if another process is writing too.
	// No O_TRUNC: that's exactly what we're guarding against.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("audit: stat %s: %w", path, err)
	}
	r.file = f
	r.size = info.Size()

	// If the last line is incomplete, add a newline first.
	//
	// O_APPEND continues writing from the end of the file. If the last power cut left only half a line, without a newline
	// the new record would be glued straight onto that fragment, so one crash would ruin not one record
	// but two: the incomplete one, and the next one that should have been intact.
	if err := r.terminateDanglingLine(path); err != nil {
		_ = f.Close()
		r.file = nil
		return err
	}

	last, reason := lastLink(path)
	r.seq = last.Seq
	r.prev = last.Hash
	switch {
	case reason != "":
		// If the chain tail can't be read, start a new chain and say why explicitly. Quietly carrying on from seq=1 would make
		// "the file is new" and "the file was truncated" impossible to tell apart.
		r.seq = 0
		r.prev = ""
		r.writeRecord(Event{
			Action:  ActionChainStarted,
			Subject: "kernel",
			Detail:  reason,
		})

	case last.Action != ActionClosed:
		// The previous run didn't end cleanly: a power cut, a KILL, or a process crash.
		//
		// The chain itself still links up, which is exactly why the missing records can't be seen. This record
		// is that gap's name: it writes "up to 1 second of records may be missing" into the chain, so the
		// verifier can report it instead of giving a false "all passed".
		//
		// Denied=true is intentional: it should be synced immediately, and it also shows up in ops queries that only filter
		// for refusals.
		r.writeRecord(Event{
			Action:  ActionUncleanShutdown,
			Subject: "kernel",
			Denied:  true,
			Detail: fmt.Sprintf(
				"last_seq=%d last_action=%s window<=%s",
				last.Seq, last.Action, minSyncInterval),
		})
	}
	return nil
}

// terminateDanglingLine adds a newline if the file doesn't end with one.
//
// After that the fragment is a line of its own: ReadFile and lastLink both skip it as an unparseable line,
// and it affects no record before or after it.
func (r *FileRecorder) terminateDanglingLine(path string) error {
	if r.size == 0 {
		return nil
	}
	tail := make([]byte, 1)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("audit: reopen %s to inspect tail: %w", path, err)
	}
	_, err = f.ReadAt(tail, r.size-1)
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("audit: read tail of %s: %w", path, err)
	}
	if closeErr != nil {
		return fmt.Errorf("audit: close %s after tail read: %w", path, closeErr)
	}
	if tail[0] == '\n' {
		return nil
	}
	n, err := r.file.Write([]byte{'\n'})
	if err != nil {
		return fmt.Errorf("audit: terminate dangling line in %s: %w", path, err)
	}
	r.size += int64(n)
	return nil
}

// lastLink reads the last complete record in the file.
//
// A non-empty second return value means "can't link to the previous chain", with a reason a person can understand.
//
// It returns the whole record and not just (seq, hash): the caller also needs its Action to judge whether the previous run
// ended cleanly (see ActionClosed).
func lastLink(path string) (Record, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Sprintf("cannot read %s: %v", path, err)
	}
	if len(data) == 0 {
		return Record{}, "audit file is empty"
	}

	// Search backwards from the end for the last complete JSON line.
	//
	// The last line may be incomplete: during a power cut a write may have landed only half. That's not tampering,
	// it's a normal crash artefact, so skip it and keep looking instead of judging the whole file untrustworthy.
	end := len(data)
	for end > 0 {
		start := end - 1
		for start > 0 && data[start-1] != '\n' {
			start--
		}
		line := data[start:end]
		var rec Record
		if err := json.Unmarshal(trimNewline(line), &rec); err == nil && rec.Hash != "" {
			return rec, ""
		}
		end = start
		for end > 0 && data[end-1] == '\n' {
			end--
		}
	}
	return Record{}, "no complete record found in audit file"
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// Record queues one audit entry. It never blocks the caller.
//
// Audit call sites are all over permission decisions, leases, installs and so on, and some are hot paths. Making them wait for a
// disk write would add audit's cost to every audited operation.
func (r *FileRecorder) Record(ctx context.Context, ev Event) {
	// The slog mirror goes first: even if the queue is full, ops can still see this one in the journal.
	r.log.LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.String("action", ev.Action),
		slog.String("subject", ev.Subject),
		slog.Bool("denied", ev.Denied),
		slog.Any("err", ev.Err),
		slog.String("detail", ev.Detail),
	)

	select {
	case r.queue <- ev:
	default:
		// Queue full. Count it; the write goroutine turns it into a ChainGap at the next successful write.
		r.dropped.Add(1)
	}
}

// Dropped is the running count of failed enqueues, for reporting at shutdown.
func (r *FileRecorder) Dropped() uint64 { return r.dropped.Load() }

// Close drains the queue, finishes writing and closes the file. Idempotent.
func (r *FileRecorder) Close() error {
	r.once.Do(func() { close(r.stop) })
	<-r.done
	return nil
}

func (r *FileRecorder) run() {
	defer close(r.done)

	// The timer only "syncs what has built up". It produces no records, and sync returns at once when dirty is false,
	// so on an idle machine this tick costs nothing.
	ticker := time.NewTicker(minSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case ev := <-r.queue:
			r.writeRecord(ev)
		case <-ticker.C:
			r.sync()
		case <-r.stop:
			// Drain: the records in the queue at shutdown are probably the most important ones (revocations, shutdown convergence).
			for {
				select {
				case ev := <-r.queue:
					r.writeRecord(ev)
				default:
					r.flushGap()
					// Final record + unconditional sync, marking this run as cleanly ended.
					// Order matters: write the record, then sync. The other way round leaves the record itself in the cache,
					// and every normal shutdown would look like a power cut.
					r.append(Event{
						Action:  ActionClosed,
						Subject: "kernel",
						Detail:  fmt.Sprintf("final_seq=%d", r.seq),
					})
					r.sync()
					if r.file != nil {
						_ = r.file.Close()
					}
					return
				}
			}
		}
	}
}

// flushGap writes the accumulated drop count as one ChainGap.
func (r *FileRecorder) flushGap() {
	total := r.dropped.Load()
	if total == r.pending {
		return
	}
	lost := total - r.pending
	r.pending = total
	r.append(Event{
		Action:  ActionChainGap,
		Subject: "kernel",
		Denied:  true,
		Detail:  fmt.Sprintf("lost=%d reason=queue_full", lost),
	})
}

// writeRecord writes one audit entry, first adding a ChainGap and rotating if needed.
func (r *FileRecorder) writeRecord(ev Event) {
	r.flushGap()
	r.rotateIfNeeded()
	r.append(ev)
	if ev.Denied {
		r.syncIfDue()
	}
}

// append is the only place that really writes to the file.
func (r *FileRecorder) append(ev Event) {
	if r.file == nil {
		return
	}
	rec, err := newRecord(r.seq+1, r.prev, r.now(), ev)
	if err != nil {
		r.log.Error("audit: build record", "action", ev.Action, "err", err)
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		r.log.Error("audit: encode record", "action", ev.Action, "err", err)
		return
	}
	line = append(line, '\n')

	n, err := r.file.Write(line)
	if err != nil {
		// The chain doesn't advance: seq and prev keep their values, and the next record continues from the current tail.
		// Advancing them would leave a record missing from the file without it being visible, which is exactly what we guard against.
		r.log.Error("audit: write record", "action", ev.Action, "err", err)
		r.dropped.Add(1)
		return
	}
	r.size += int64(n)
	r.seq = rec.Seq
	r.prev = rec.Hash
	r.dirty = true
	r.written.Add(1)
}

// sync really puts written records onto the storage medium.
//
// Write returning doesn't mean it's on disk: the content sits in the kernel page cache, and Linux writes back after up to 30 seconds by default.
// In a power cut those 30 seconds of records vanish, and the remaining chain is entirely self-consistent, so the verifier would report "pass".
// That's the worst kind of failure: it gives false certainty.
//
// Called only by the write goroutine. Returns at once when dirty is false, without touching the device.
func (r *FileRecorder) sync() {
	if r.file == nil || !r.dirty {
		return
	}
	if err := r.file.Sync(); err != nil {
		// A failed sync doesn't advance lastSync or clear dirty: it has to be retried next time.
		// Clearing them would mean "thinks it's on disk but it isn't", which is more dangerous than not syncing.
		r.log.Error("audit: fsync", "err", err)
		return
	}
	r.dirty = false
	r.lastSync = r.now()
	r.synced.Add(1)
}

// Stats are counters for ops and the shutdown log.
//
// records is how many entries have reached the file, syncs is the fsync count. Their ratio shows how well batching
// works; when syncs approaches records, it's worth looking at what keeps being refused.
func (r *FileRecorder) Stats() (records, syncs uint64) {
	return r.written.Load(), r.synced.Load()
}

// syncIfDue is the immediate sync path for Denied records, throttled by minSyncInterval.
//
// Refusals are the most important kind of audit (attempts to exceed permissions, failed signatures, revocations), so sync them
// straight away when possible. But a run of refusals, say one package being refused over and over, shouldn't become back-to-back fsyncs;
// that's the old per-record sync problem. A record held back by the throttle is covered by the timer and synced within 1 second.
func (r *FileRecorder) syncIfDue() {
	if !r.dirty {
		return
	}
	if r.now().Sub(r.lastSync) < minSyncInterval {
		return
	}
	r.sync()
}

// rotateIfNeeded rotates when over the threshold, and continues the old chain at the start of the new file.
func (r *FileRecorder) rotateIfNeeded() {
	if r.file == nil || r.size < r.maxBytes {
		return
	}
	carry := r.prev
	carrySeq := r.seq

	// It must sync before renaming. Close doesn't imply fsync: the old file would become .1 with an unsynced tail,
	// and after a power cut that part would be gone, while the chain before it is still self-consistent and the verifier wouldn't notice anything missing.
	// No minSyncInterval throttling here; rotation is infrequent anyway.
	r.sync()
	if err := r.file.Close(); err != nil {
		r.log.Error("audit: close before rotate", "err", err)
	}
	r.file = nil

	current := filepath.Join(r.dir, FileName)
	// Shift from the oldest back, to avoid overwriting. The one beyond maxFiles is dropped.
	oldest := filepath.Join(r.dir, fmt.Sprintf("%s.%d", FileName, r.maxFiles))
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		r.log.Warn("audit: remove oldest rotated file", "path", oldest, "err", err)
	}
	for i := r.maxFiles - 1; i >= 1; i-- {
		from := filepath.Join(r.dir, fmt.Sprintf("%s.%d", FileName, i))
		to := filepath.Join(r.dir, fmt.Sprintf("%s.%d", FileName, i+1))
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			r.log.Warn("audit: rotate", "from", from, "to", to, "err", err)
		}
	}
	if err := os.Rename(current, filepath.Join(r.dir, FileName+".1")); err != nil {
		r.log.Error("audit: rotate current file", "err", err)
	}

	f, err := os.OpenFile(current, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		// Can't open the new file after rotating: from now on audit only goes to slog. That's a hard fault,
		// logged at Error so it stands out in the journal.
		r.log.Error("audit: open new file after rotate", "err", err)
		return
	}
	r.file = f
	r.size = 0

	// The first record of the new file carries the old chain's last hash. Without it, to the verifier every rotation would be
	// a "seq restarts at 1", indistinguishable from a truncation attack.
	//
	// seq keeps increasing instead of resetting: the chain is one chain across files, and the files are just how it's stored.
	r.append(Event{
		Action:  ActionRotated,
		Subject: "kernel",
		Detail:  fmt.Sprintf("carried_seq=%d carried_hash=%s", carrySeq, carry),
	})
}

// ReadFile reads every complete record in an audit file, for the verifier.
//
// skipped is the number of unparseable lines.
//
// # Why unparseable lines are only counted, not reported as errors
//
// During a power cut a write may land only half a line. The next start adds a newline so it becomes a line of its own
//
// (see terminateDanglingLine), so an unparseable fragment is left in the middle of the file. That's a
//
// crash artefact, not tampering, and it doesn't affect the chain: that record was never fully written,
// and the following record links to the last complete one before it.
//
// But it must be reported. A non-zero skipped means this machine had an abnormal shutdown, which ops
// need to know. Skipping it quietly would make the audit look clean all along.
//
// Real tampering (deleting records, changing content, reordering) is caught by VerifyChain and has nothing to do with this function.
func ReadFile(path string) (records []Record, skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: read %s: %w", path, err)
	}

	start := 0
	for i := 0; i <= len(data); i++ {
		if i != len(data) && data[i] != '\n' {
			continue
		}
		line := trimNewline(data[start:i])
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			skipped++
			continue
		}
		records = append(records, rec)
	}
	return records, skipped, nil
}
