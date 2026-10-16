package safety

import "sync/atomic"

// eventKind is the fixed classification of audit events, for the zero-allocation audit ring.
type eventKind uint8

const (
	evTripped               eventKind = iota + 1 // latched by a Trip (observed by the Supervisor)
	evHaltDelivered                              // the Stop Lane delivered the fixed stop signal
	evDeliveryFault                              // the Stop Lane's delivery failed
	evStopProgress                               // StopProgress phase advanced
	evProviderAcceptTimeout                      // provider_accept_timeout triggered an escalation
	evDeviceAckTimeout                           // device_stop_ack_timeout triggered an escalation
	evStandstillTimeout                          // standstill_timeout triggered an escalation
	evStandstillConfirmed                        // trustworthy standstill evidence obtained
	evRearm                                      // back to NORMAL through an explicit re-arm
)

func (k eventKind) String() string {
	switch k {
	case evTripped:
		return "tripped"
	case evHaltDelivered:
		return "halt_delivered"
	case evDeliveryFault:
		return "delivery_fault"
	case evStopProgress:
		return "stop_progress"
	case evProviderAcceptTimeout:
		return "provider_accept_timeout"
	case evDeviceAckTimeout:
		return "device_ack_timeout"
	case evStandstillTimeout:
		return "standstill_timeout"
	case evStandstillConfirmed:
		return "standstill_confirmed"
	case evRearm:
		return "rearm"
	default:
		return "unknown"
	}
}

// eventRecord is a fixed-size, pointer-free audit record: it can be preallocated as a block and copied into the ring by value,
// so writes allocate nothing. Stringification is left to the normal-priority auditDrain.
type eventRecord struct {
	kind   eventKind
	reason ReasonCode
	phase  StopPhase
	epoch  uint64
}

// auditRing is a bounded, lock-free multi-producer single-consumer queue (the MPSC use of Vyukov's bounded MPMC
// queue):
//
//   - Producers = the Stop Lane (FIFO 95) + Supervisor (FIFO 90), both on RT threads' hot paths;
//     push takes no lock, allocates nothing and never blocks. When the ring is full it drops and counts (dropped), because audit can be rate-limited,
//     while blocking the RT stop path would directly delay the safety response.
//   - Consumer = auditDrain (normal priority SCHED_OTHER), which stringifies and stores after pop.
//
// Each cell carries a seq number that acts as a latch for which round the cell currently belongs to and whether it's writable or readable,
// so it can be handed over safely without a mutex. size must be a power of 2.
type auditRing struct {
	buf     []ringCell
	mask    uint64
	enqPos  atomic.Uint64 // producers' write cursor (monotonic)
	deqPos  atomic.Uint64 // the consumer's read cursor (monotonic)
	dropped atomic.Uint64
}

type ringCell struct {
	seq atomic.Uint64
	rec eventRecord
}

// newAuditRing builds a ring of capacity size (must be a power of 2) and initialises each cell's seq to
// its index, the empty-queue starting condition of Vyukov's queue.
func newAuditRing(size int) *auditRing {
	r := &auditRing{buf: make([]ringCell, size), mask: uint64(size - 1)}
	for i := range r.buf {
		r.buf[i].seq.Store(uint64(i))
	}
	return r
}

// push enqueues one record. Returns true on success; when the ring is full it returns false and increments dropped.
// Lock-free, allocation-free, callable on RT hot paths.
func (r *auditRing) push(rec eventRecord) bool {
	for {
		pos := r.enqPos.Load()
		cell := &r.buf[pos&r.mask]
		seq := cell.seq.Load()
		dif := int64(seq) - int64(pos)
		switch {
		case dif == 0:
			// The cell is free and it's this round's turn to write: claim the write cursor.
			if r.enqPos.CompareAndSwap(pos, pos+1) {
				cell.rec = rec
				cell.seq.Store(pos + 1) // publish: seq=pos+1 means readable
				return true
			}
		case dif < 0:
			// The cell hasn't been read by the consumer yet (seq is behind): the queue is full.
			r.dropped.Add(1)
			return false
		default:
			// Another producer has already advanced enqPos; reread and retry.
		}
	}
}

// pop dequeues one record (called only by the single consumer). When empty it returns (zero value, false).
func (r *auditRing) pop() (eventRecord, bool) {
	for {
		pos := r.deqPos.Load()
		cell := &r.buf[pos&r.mask]
		seq := cell.seq.Load()
		dif := int64(seq) - int64(pos+1)
		switch {
		case dif == 0:
			// The cell has been published by a producer and it's this round's turn to read: claim the read cursor.
			if r.deqPos.CompareAndSwap(pos, pos+1) {
				rec := cell.rec
				cell.seq.Store(pos + r.mask + 1) // return it: mark it writable for the next round
				return rec, true
			}
		case dif < 0:
			// The cell hasn't been published yet: the queue is empty.
			return eventRecord{}, false
		default:
		}
	}
}

func (r *auditRing) droppedCount() uint64 { return r.dropped.Load() }
