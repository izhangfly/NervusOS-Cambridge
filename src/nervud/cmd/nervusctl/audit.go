package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nervus-os/nervud/internal/audit"
)

// cmdAuditVerify verifies the audit hash chain.
//
// # The question it answers
//
// "Has this audit been changed since I last looked at it?"
//
// Verification spans files: rotated history files and the current file are one chain, and a new file's first entry carries
// the old file's last hash. Verifying each file on its own would raise false alarms at every file boundary.
//
// # The question it doesn't answer
//
// "Did someone rewrite the whole audit and recompute the entire chain?" Answering that needs an external anchor such as remote logs, a TPM
// or write-once media; a local verifier has none of those anchors, so it doesn't claim to cover that.
func cmdAuditVerify(args []string, stdout io.Writer) error {
	dir := audit.DefaultDir
	switch len(args) {
	case 0:
	case 1:
		dir = args[0]
	default:
		return usageErr{msg: "audit-verify [dir]"}
	}

	files, err := auditFilesOldestFirst(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("nervusctl: no audit files in %s", dir)
	}

	// prev/seq run across files, so verification must start from the oldest file.
	//
	// The starting point can't be assumed to be seq 1: the rotation retention policy discards the oldest files, so in steady state the oldest
	// available file starts somewhere in the middle of the chain. Insisting on verifying from 1 would report a normal rotation reclaim as
	// "records were deleted", the worst kind of false alarm, which makes real alerts get treated as noise.
	//
	// So the first entry of the oldest file is the anchor, and the end says clearly how far the verification covers.
	prev := ""
	seq := uint64(0)
	anchored := false
	total := 0
	skippedTotal := 0
	// Where unclean shutdowns happened. The chain is still consistent at these points, but the tail may be missing records;
	// without reporting them, "verified" would claim more than it means.
	var unclean []audit.Record

	for _, path := range files {
		records, skipped, err := audit.ReadFile(path)
		if err != nil {
			return err
		}
		skippedTotal += skipped
		if !anchored && len(records) > 0 {
			prev = records[0].Prev
			seq = records[0].Seq
			anchored = true
		}
		if idx, err := audit.VerifyChain(records, prev, seq); err != nil {
			// Report the file and position: when something is wrong, what matters most is "from which entry it stopped matching",
			// which more or less points to the time window of the event.
			outf(stdout, "FAIL  %s\n", filepath.Base(path))
			if idx >= 0 && idx < len(records) {
				outf(stdout, "      at record seq=%d action=%s\n",
					records[idx].Seq, records[idx].Action)
			}
			return fmt.Errorf("nervusctl: audit chain broken in %s: %w", path, err)
		}
		if n := len(records); n > 0 {
			prev = records[n-1].Hash
			seq = records[n-1].Seq + 1
		}
		total += len(records)
		for _, rec := range records {
			if rec.Action == audit.ActionUncleanShutdown {
				unclean = append(unclean, rec)
			}
		}
		outf(stdout, "ok    %-20s %d records\n", filepath.Base(path), len(records))
	}

	if !anchored {
		return fmt.Errorf("nervusctl: no complete audit records in %s", dir)
	}

	// Say clearly which stretch was verified. A bare "chain verified" without a starting point makes people
	// think the whole history was checked, while the part reclaimed by rotation can't be checked by anyone.
	firstRecords, _, err := audit.ReadFile(files[0])
	if err != nil {
		return err
	}
	start := firstRecords[0]
	outf(stdout, "\nchain verified: %d records, seq %d..%d\n", total, start.Seq, seq-1)
	if start.Seq != 1 || start.Prev != "" {
		outf(stdout,
			"note: records before seq %d were rotated out; this run cannot verify them\n",
			start.Seq)
	}
	if skippedTotal > 0 {
		// A fragment isn't tampering, but it must be reported: it means an unclean shutdown happened on this machine.
		// Skipping it quietly would make the audit look clean forever.
		outf(stdout, "note: %d partial record(s) skipped; the machine was not shut down cleanly\n",
			skippedTotal)
	}
	if len(unclean) > 0 {
		// "The chain is intact" doesn't mean "nothing is missing".
		//
		// A power loss drops the last few records written but not yet fsynced, and the remaining chain is fully consistent:
		// sequence numbers continuous, prev links matching, hashes all correct. Without reporting these points, the "chain verified" above
		// becomes false certainty: it only proves "what's left wasn't changed", not
		// "everything that should be there is there".
		outf(stdout,
			"note: %d unclean shutdown(s); records may be missing just before each\n",
			len(unclean))
		for _, rec := range unclean {
			outf(stdout, "      seq=%d %s\n", rec.Seq, rec.Detail)
		}
	}
	return nil
}

// auditFilesOldestFirst lists the audit files in the chain's time order.
//
// Rotation moves the current file to .1, .1 to .2 and so on, so a bigger number is older,
// and the chain must be verified from the oldest.
func auditFilesOldestFirst(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("nervusctl: read %s: %w", dir, err)
	}

	var rotated []string
	hasCurrent := false
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case name == audit.FileName:
			hasCurrent = true
		case strings.HasPrefix(name, audit.FileName+"."):
			rotated = append(rotated, name)
		}
	}

	// Sort by suffix number, descending: .3 is older than .2.
	sort.Slice(rotated, func(i, j int) bool {
		return rotationIndex(rotated[i]) > rotationIndex(rotated[j])
	})

	files := make([]string, 0, len(rotated)+1)
	for _, name := range rotated {
		files = append(files, filepath.Join(dir, name))
	}
	if hasCurrent {
		files = append(files, filepath.Join(dir, audit.FileName))
	}
	return files, nil
}

func rotationIndex(name string) int {
	suffix := strings.TrimPrefix(name, audit.FileName+".")
	n := 0
	for _, c := range suffix {
		if c < '0' || c > '9' {
			// Unrecognised suffixes go first (treated as oldest). Don't guess an order:
			// a wrong guess would start verification in the middle and report a false chain break.
			return 1 << 30
		}
		n = n*10 + int(c-'0')
	}
	return n
}
