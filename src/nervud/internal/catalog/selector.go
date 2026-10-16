package catalog

import (
	"sort"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// SelectResources picks resources according to a ResourceSelector.
//
// # Matching rules
//
//   - type: exact match when non-empty
//   - role: exact match when non-empty
//   - labels: every key and value must match (AND); an empty map doesn't filter
//
// # What about multiple candidates
//
// The policy decides; if unspecified it fails closed as REQUIRE_UNIQUE.
//
// For a robot, "I asked for a camera and the system gave me any old one" is far more dangerous than a clear error,
// especially when the candidates mix front and rear cameras, or left and right arms. If you want the system to choose for you, say so explicitly.
//
// SYSTEM_PREFERRED takes the one with the lexicographically smallest stable_role. It picks a deterministic one rather than the first
// from iterating a map: Go's map iteration order is randomised, so the same Catalog would pick different devices on two
// boots, and that kind of nondeterminism is extremely hard to reproduce in the field.
func SelectResources(
	snapshot *Snapshot, sel *ipcv1.ResourceSelector,
) ([]ResourceDefinition, bool) {
	if snapshot == nil {
		return nil, false
	}

	if sel == nil {
		// In v2 an empty selector no longer implicitly means the chassis. No target means no target.
		return nil, false
	}
	matched := FilterResources(snapshot, sel.GetType(), sel.GetRole(), sel.GetLabels())
	if len(matched) == 0 {
		return nil, false
	}

	switch sel.GetPolicy() {
	case ipcv1.ResourceSelectionPolicy_RESOURCE_SELECTION_POLICY_SYSTEM_PREFERRED:
		return matched[:1], true
	case ipcv1.ResourceSelectionPolicy_RESOURCE_SELECTION_POLICY_REQUIRE_UNIQUE,
		ipcv1.ResourceSelectionPolicy_RESOURCE_SELECTION_POLICY_UNSPECIFIED:
		if len(matched) != 1 {
			return matched, false
		}
		return matched, true
	default:
		// Unknown policies fail closed. When the protocol adds a value this build doesn't know,
		// treat it as strictly as possible instead of guessing.
		return matched, false
	}
}

// FilterResources lists every match without deciding "which of several candidates".
//
// It's separate from SelectResources because they answer different questions. SelectResources answers
// "which one do I connect to", where multiple candidates are an ambiguity that must be resolved; FilterResources answers
// "what is there", where multiple candidates are the answer itself. Making enumeration go through a policy would mean forcing
// SYSTEM_PREFERRED on it, which would make "list every camera" return one.
//
// The order is lexicographic by (ResourceType, StableRole). Go's map iteration order is randomised,
// so returning the iteration result directly would give the same Catalog different orders on two calls; in the UI
// the device list jumps on every refresh, and almost nobody would report that as a bug.
func FilterResources(
	snapshot *Snapshot,
	resourceType string,
	stableRole string,
	labels map[string]string,
) []ResourceDefinition {
	if snapshot == nil {
		return nil
	}
	var matched []ResourceDefinition
	for _, def := range snapshot.resources {
		if !matchesFields(def, resourceType, stableRole, labels) {
			continue
		}
		matched = append(matched, cloneResourceDefinition(def))
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].ResourceType != matched[j].ResourceType {
			return matched[i].ResourceType < matched[j].ResourceType
		}
		return matched[i].StableRole < matched[j].StableRole
	})
	return matched
}

// matchesFields is the one implementation of the matching rules, shared by SelectResources and FilterResources.
//
// An empty field doesn't filter; labels must match every key and value (AND).
func matchesFields(
	def ResourceDefinition,
	resourceType string,
	stableRole string,
	labels map[string]string,
) bool {
	if resourceType != "" && def.ResourceType != resourceType {
		return false
	}
	if stableRole != "" && def.StableRole != stableRole {
		return false
	}
	for key, want := range labels {
		got, ok := def.Labels[key]
		if !ok || got != want {
			return false
		}
	}
	return true
}

// SelectorIsEmpty reports whether the selector specifies nothing, in which case the caller falls back to the interface's
// default resource. labels or policy on their own count as "specified": a selector with only labels
// is an explicit semantic query and shouldn't be treated as empty.
func SelectorIsEmpty(sel *ipcv1.ResourceSelector) bool {
	return sel == nil ||
		(sel.GetType() == "" &&
			sel.GetRole() == "" &&
			len(sel.GetLabels()) == 0 &&
			sel.GetPolicy() == ipcv1.ResourceSelectionPolicy_RESOURCE_SELECTION_POLICY_UNSPECIFIED)
}
