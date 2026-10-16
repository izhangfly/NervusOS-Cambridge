// This file is the hash chain over audit records: it makes local tampering leave traces.
//
// # What it protects against, and what it doesn't
//
// Protects against: deleting a few records in the middle, changing one record's subject, swapping two records, changing a record's
// denied=true to false. Any change makes the prev of every later record mismatch,
// and recomputing means rewriting everything after it.
//
// Doesn't protect against: someone with root rewriting the whole file and recomputing the whole chain. That needs an external anchor
//
// (remote logging, a TPM, write-once media); this file doesn't pretend it can do that.
//
// The goal is tampering that's visible, not tampering that's impossible: most of the time audit gets altered by mistake,
// a buggy log rotation script, or an ill-considered "quick clean-up", not a planned attack.
// This file exposes all of those cases.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// chainVersion goes into every record.
//
// It must be in the chain: the verifier has to tell "I can't compute this record" from "this record was altered".
// Without a version number, one format change would make the whole history look tampered with.
const chainVersion = 1

// Record is one audit entry on disk, one line of JSON Lines.
//
// Field order is JSON key order (encoding/json writes fields in struct order), and the hash is over the
// serialised bytes, so field order is part of the chain and reordering would invalidate all history.
type Record struct {
	// V is the chain format version.
	V int `json:"v"`

	// Seq starts at 1, increases monotonically and is never reused. 0 is the "no predecessor" sentinel.
	Seq uint64 `json:"seq"`

	// Prev is the previous record's Hash (hex). Empty for the first record.
	Prev string `json:"prev"`

	// TimeUnixNanos is when the record was written (wall clock).
	//
	// Wall clock rather than monotonic: audit has to answer "what time did it happen", which must line up with external events
	// (tickets, monitoring alerts, field notes). A monotonic clock lines up with nothing.
	//
	// The cost is that the wall clock can be adjusted, so it takes part in no decision; it's only for people to read and correlate.
	TimeUnixNanos int64 `json:"t"`

	Action  string `json:"action"`
	Subject string `json:"subject"`
	Denied  bool   `json:"denied"`
	Err     string `json:"err,omitempty"`
	Detail  string `json:"detail,omitempty"`

	// Hash is the SHA-256 of this record (excluding this field), in hex.
	Hash string `json:"hash"`
}

// hashInput is the part that's hashed: every field except Hash.
//
// It's defined separately instead of blanking Record's Hash and serialising: blanking depends on
// `json:"hash,omitempty"`, and if anyone ever removed that omitempty, all history would become invalid
// at the same moment, with no compile error.
type hashInput struct {
	V             int    `json:"v"`
	Seq           uint64 `json:"seq"`
	Prev          string `json:"prev"`
	TimeUnixNanos int64  `json:"t"`
	Action        string `json:"action"`
	Subject       string `json:"subject"`
	Denied        bool   `json:"denied"`
	Err           string `json:"err,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// newRecord links a new record to the given predecessor and computes its hash.
func newRecord(seq uint64, prev string, at time.Time, ev Event) (Record, error) {
	rec := Record{
		V:             chainVersion,
		Seq:           seq,
		Prev:          prev,
		TimeUnixNanos: at.UnixNano(),
		Action:        ev.Action,
		Subject:       ev.Subject,
		Denied:        ev.Denied,
		Detail:        ev.Detail,
	}
	if ev.Err != nil {
		rec.Err = ev.Err.Error()
	}
	sum, err := rec.computeHash()
	if err != nil {
		return Record{}, err
	}
	rec.Hash = sum
	return rec, nil
}

func (r Record) computeHash() (string, error) {
	wire, err := json.Marshal(hashInput{
		V:             r.V,
		Seq:           r.Seq,
		Prev:          r.Prev,
		TimeUnixNanos: r.TimeUnixNanos,
		Action:        r.Action,
		Subject:       r.Subject,
		Denied:        r.Denied,
		Err:           r.Err,
		Detail:        r.Detail,
	})
	if err != nil {
		return "", fmt.Errorf("audit: encode record for hashing: %w", err)
	}
	sum := sha256.Sum256(wire)
	return hex.EncodeToString(sum[:]), nil
}

// VerifyChain checks a run of consecutive records. It returns the index of the first bad record and the reason.
//
// expectedPrev is the Hash of the record before this run; pass an empty string when checking a file from the start.
// expectedSeq likewise; pass 1 when checking from the start.
//
// It reports a position rather than just saying "the chain is broken": when audit goes wrong, what matters most is "from which record
// it stops being right", which basically pins down the time window of the incident.
func VerifyChain(records []Record, expectedPrev string, expectedSeq uint64) (int, error) {
	prev := expectedPrev
	seq := expectedSeq

	for i, rec := range records {
		if rec.V != chainVersion {
			return i, fmt.Errorf("audit: record %d has chain version %d, this build understands %d",
				rec.Seq, rec.V, chainVersion)
		}
		if rec.Seq != seq {
			// A sequence jump = records were deleted or inserted.
			return i, fmt.Errorf("audit: record seq %d, want %d (records removed or inserted)",
				rec.Seq, seq)
		}
		if rec.Prev != prev {
			return i, fmt.Errorf("audit: record %d has prev %q, want %q (chain broken)",
				rec.Seq, rec.Prev, prev)
		}
		want, err := rec.computeHash()
		if err != nil {
			return i, err
		}
		if want != rec.Hash {
			// The content was altered: fields changed but the hash wasn't recomputed.
			return i, fmt.Errorf("audit: record %d hash mismatch (content modified)", rec.Seq)
		}
		prev = rec.Hash
		seq++
	}
	return -1, nil
}
