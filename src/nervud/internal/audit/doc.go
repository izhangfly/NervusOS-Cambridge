// Package audit is nervud's security audit: who was allowed or refused what, and when.
//
// # Two implementations
//
//	New(log)               writes only to slog. For tests and minimal wiring.
//	NewFileRecorder(cfg)   append-only hash-chained file + a slog mirror. For production.
//
// The latter adds three things slog can't do: it isn't filtered out by the log level, isn't overwritten by rotation,
// and shows when it has been altered.
//
// # Dependency direction
//
// This package imports no sibling module. Dependency arrows always point at audit, so an import cycle is structurally
// impossible. That's what lets permissions, leases, installs and Safety all call it.
//
// # Limits of the integrity protection
//
// The hash chain makes local tampering leave traces: deleting a few records, changing one, or reordering them makes the prev of
// every later record mismatch.
//
// It doesn't protect against someone with root rewriting the whole file and recomputing the whole chain; that needs an external anchor
//
// (remote logging, a TPM, write-once media). The goal is tampering that's visible, not tampering that's impossible.
//
// Most of the time audit gets altered by mistake, a buggy log clean-up script, or an ill-considered
// "tidy-up", not a planned attack. This package exposes all of those cases.
//
// # How much a power cut loses
//
// Write returning doesn't mean it's on disk: the content sits in the kernel page cache, and Linux writes back after up to 30 seconds by default.
// So this package handles fsync itself, batching with refusals first:
//
//   - a timer syncs every minSyncInterval (1 second), touching the device only when there are unsynced records
//   - Denied records are synced at once, but at least 1 second apart
//   - before rotation and at shutdown it syncs unconditionally, with no throttling
//
// It doesn't fsync every record because that backfires: one fsync on an SD card takes a few to tens of milliseconds, so doing it per record would
// cut the write goroutine's drain rate to a few dozen records a second, and the queue overflows in a burst: trying not to lose records loses them.
//
// # How you can tell something was lost
//
// After a power cut loses the last few records, the remaining chain is entirely self-consistent: sequence numbers continuous, prevs linked, every hash right.
// With only the hash chain, the verifier would report "pass" on an incomplete file, which isn't safety, it's
// false certainty.
//
// So a clean shutdown writes an ActionClosed record at the end. If the next start finds the chain doesn't end with it, it adds an
// ActionUncleanShutdown, writing "up to 1 second of records may be missing here" into the chain.
//
// Each of the three anomalies has its own name on the chain, so none can pass for another:
//
//	ActionChainStarted      the file is new / was deleted and recreated; sequence numbers restart at 1
//	ActionUncleanShutdown   power cut or KILL; the chain is unbroken but the tail may be missing records
//	ActionChainGap          the queue was full, and exactly how many records were lost is known
//
// # How to verify
//
//	nervusctl audit-verify [dir]
//
// It verifies across files (rotated history files and the current file are one chain), and says clearly which part
// the check covered. Nobody can verify the part rotation has recycled, and the tool says so instead of pretending it checked it. Likewise,
// it lists the position of every UncleanShutdown: "the chain is intact" doesn't mean "nothing is missing".
package audit
