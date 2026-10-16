// This file defines the aggregated Status/Report data model, wires health into the kernel.Module
// lifecycle, and implements the single aggregation logic for Report
package health

import (
	"context"

	"github.com/nervus-os/nervud/internal/control"
	"github.com/nervus-os/nervud/internal/motiongate"
	"github.com/nervus-os/nervud/internal/safety"
	"github.com/nervus-os/nervud/internal/service"
)

// Status is the aggregated overall health level: the single authority on "this machine's status in one word"
type Status uint8

const (
	// StatusHealthy Safety is NORMAL and no component is tripped
	StatusHealthy Status = iota
	// StatusDegraded Safety is NORMAL, but at least one non-fatal component has tripped
	//  (a Vital component tripping triggers a Safety Trip synchronously in service.onCircuitBreak,
	// so whatever remains at this level must be non-Vital; health doesn't need to judge
	// Criticality again, since the Safety state already implies the conclusion; see deriveStatus)
	StatusDegraded
	// StatusFault Safety isn't NORMAL (latched / recovering / waiting for re-arm),
	// or the aggregator itself isn't wired up correctly (nil Observer, fail-closed)
	StatusFault
)

func (s Status) String() string {
	switch s {
	case StatusHealthy:
		return "HEALTHY"
	case StatusDegraded:
		return "DEGRADED"
	default:
		return "FAULT"
	}
}

// Report is the full snapshot of one Report call: the overall verdict + the raw snapshots of the three authoritative sources
//
// Safety/Control/Components reuse the types their packages already export, without a second projection:
// health doesn't understand these fields better than their sources, and restating them would only risk drift
type Report struct {
	Status     Status
	Safety     safety.Snapshot
	Control    control.Snapshot
	Components []service.Instance
}

// ControlObserver is a narrow interface to control.Module: read a consistent control-plane snapshot
type ControlObserver interface {
	ControlSnapshot() control.Snapshot
}

// ServiceObserver is a narrow interface to service.Manager: read-only snapshots of every known component instance
type ServiceObserver interface {
	Instances() []service.Instance
}

// Module wires health into the kernel.Module lifecycle and holds three read-only observer interfaces
//
// In v1 Start/Stop are no-ops: it holds no state of its own, each Report call
// reads the three authoritative sources fresh, and there's no background loop to start or anything to wrap up at shutdown
type Module struct {
	safety  safety.Observer // reuses safety's own Observer rather than reinventing it
	control ControlObserver
	service ServiceObserver
}

// New constructs the health Module. The three observers are satisfied implicitly by *safety.Module/*control.Module/
// *service.Manager (the main.go wiring pattern)
func New(safetyObs safety.Observer, controlObs ControlObserver, serviceObs ServiceObserver) *Module {
	return &Module{safety: safetyObs, control: controlObs, service: serviceObs}
}

func (m *Module) Name() string { return "health" }

// Start has no initialisation to do: Report reads and computes fresh, with no state to warm up
func (m *Module) Start(_ context.Context) error { return nil }

// Stop: pure read-only aggregation, nothing to release
func (m *Module) Stop(_ context.Context) error { return nil }

// Report reads the three authoritative sources fresh and combines them into one aggregated snapshot. Unwired fields (nil) fail safe:
// a missing observer takes part with a "zero-value snapshot", and deriveStatus's fail-closed rule
// guarantees "can't see the Safety state" is never mistaken for Healthy
func (m *Module) Report() Report {
	if m == nil {
		return Report{Status: StatusFault}
	}

	var safetySnap safety.Snapshot
	if m.safety != nil {
		safetySnap = m.safety.SafetySnapshot()
	}
	var controlSnap control.Snapshot
	if m.control != nil {
		controlSnap = m.control.ControlSnapshot()
	}
	var comps []service.Instance
	if m.service != nil {
		comps = m.service.Instances()
	}

	return Report{
		Status:     deriveStatus(safetySnap, comps),
		Safety:     safetySnap,
		Control:    controlSnap,
		Components: comps,
	}
}

// deriveStatus is the single implementation of the Status verdict: Safety takes precedence over component trips
//
// An uninitialised / zero-value safety.Snapshot has State motiongate.StateInvalid, which is naturally
// != StateNormal and falls into the Fault branch, consistent with motiongate's own convention of a "zero-value Gate failing
// closed twice", so health needs no extra nil check.
//
// It doesn't read Instance.Crit (pkgregistry.Criticality) and doesn't need to import
// pkgregistry: a Vital component tripping already calls m.safety.Trip synchronously in service.onCircuitBreak,
// so when this function sees Safety still NORMAL it can safely conclude "none of the current
// StateFailed components is Vital", without judging Criticality itself
func deriveStatus(s safety.Snapshot, comps []service.Instance) Status {
	if s.State != motiongate.StateNormal {
		return StatusFault
	}
	// Read fields by index rather than copying elements: service.Instance embeds a sync.Once,
	// and copying each element with range would be flagged by go vet as copying a lock
	for i := range comps {
		if comps[i].State == service.StateFailed {
			return StatusDegraded
		}
	}
	return StatusHealthy
}
