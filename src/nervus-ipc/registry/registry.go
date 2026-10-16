// Package registry **extracts** the Method Registry from generated protobuf descriptors,
// and provides helpers for checking Provider Descriptor namespace binding.
//
// It's the executable evidence of WP-A4's "data-driven, not hard-coded" (`00` red line #4): methods'
// permission/risk/confirmation metadata is the method_meta option attached in the proto to method_id enum values
// (method_registry.proto), and this package only **reads** those descriptors, writing no list of its own.
// So adding a method or an OEM interface only needs the option attached in its .proto and buf generate rerun,
// and the extraction result includes it automatically; nervud's Method Registry / endpoint interface directory update with it,
// with **not one line of kernel Go changed**.
//
// Direction of the source of truth (Agent document §4.3): Agent tool metadata only comes from descriptors extracted here, distributed with
// signatures and verified by nervud; Providers **can't lower it by self-reporting** at run time.
package registry

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// ExtractMethodMetas walks every value of a method_id enum, reads the method_meta option attached to it,
// and returns the interface's method-level directory. Values without the option (such as
// *_UNSPECIFIED = 0) are skipped.
//
// The extraction logic is exactly the same for standard and OEM interfaces: it only knows protobuf's generic extension mechanism,
// not whether the interface name contains "acme". That's why "adding an interface doesn't change the kernel" holds.
func ExtractMethodMetas(enum protoreflect.EnumDescriptor) ([]*ipcv1.MethodMeta, error) {
	var out []*ipcv1.MethodMeta
	values := enum.Values()
	for i := 0; i < values.Len(); i++ {
		val := values.Get(i)
		opts := val.Options()
		if opts == nil || !proto.HasExtension(opts, ipcv1.E_MethodMeta) {
			continue
		}
		mm, ok := proto.GetExtension(opts, ipcv1.E_MethodMeta).(*ipcv1.MethodMeta)
		if !ok || mm == nil {
			continue
		}
		// Convention: the enum value number is the method_id. A mismatch means the .proto is wrong,
		// so extraction fails closed, keeping drift out of dispatch.
		if uint32(val.Number()) != mm.GetMethodId() {
			return nil, fmt.Errorf(
				"registry: enum value %q number %d != method_meta.method_id %d",
				val.Name(), val.Number(), mm.GetMethodId())
		}
		// Descriptor options belong to the shared global descriptor. Return a deep copy, so a caller
		// modifying a MethodMeta can't pollute other catalog consumers in the same process.
		out = append(out, proto.Clone(mm).(*ipcv1.MethodMeta))
	}
	return out, nil
}

// ExtractEventMetas follows the same rules as ExtractMethodMetas, reading the event_meta option.
//
// Events and methods use two separate extension numbers, so they can be attached to **the same enum** without interfering:
// declaring method_ids and event_ids together in one interface is common, and extraction takes each its own.
func ExtractEventMetas(enum protoreflect.EnumDescriptor) ([]*ipcv1.EventMeta, error) {
	var out []*ipcv1.EventMeta
	values := enum.Values()
	for i := 0; i < values.Len(); i++ {
		val := values.Get(i)
		opts := val.Options()
		if opts == nil || !proto.HasExtension(opts, ipcv1.E_EventMeta) {
			continue
		}
		em, ok := proto.GetExtension(opts, ipcv1.E_EventMeta).(*ipcv1.EventMeta)
		if !ok || em == nil {
			continue
		}
		// Same convention as methods: the enum value number is the event_id, and a mismatch means the .proto is wrong
		if uint32(val.Number()) != em.GetEventId() {
			return nil, fmt.Errorf(
				"registry: enum value %q number %d != event_meta.event_id %d",
				val.Name(), val.Number(), em.GetEventId())
		}
		out = append(out, proto.Clone(em).(*ipcv1.EventMeta))
	}
	return out, nil
}

// ValidateOEMNamespace is the executable reference implementation of the "namespace binding" item of nervud's §7.3 independent checks
// (`00` red line #3: OEM private interfaces/permissions/resource types must be under the definer's namespace).
//
// It does **not** check signatures/trust profiles/minimum protection levels; those are nervud's job, needing
// signature and policy state held by the kernel. It only covers the one rule decidable purely from the Descriptor itself:
// every id outside the nervus.* / perm.* platform namespaces (i.e. OEM-private) must have the
// package_id namespace as its prefix. The platform namespaces only get a "shape exemption" here; callers must still
// use nervud's trust/signer conclusions to judge whether the definer may occupy them.
//
// Returns every violation; an empty slice means the namespaces are self-consistent.
func ValidateOEMNamespace(d *ipcv1.ProviderDescriptor) []error {
	var errs []error
	ns := d.GetPackageId()

	isStd := func(id string) bool { return strings.HasPrefix(id, "nervus.") }
	underNS := func(id string) bool {
		return ns != "" && strings.HasPrefix(id, ns+".")
	}

	for _, iface := range d.GetInterfaces() {
		id := iface.GetInterfaceId()
		if !isStd(id) && !underNS(id) {
			errs = append(errs, fmt.Errorf(
				"interface_id %q is private but not under package namespace %q", id, ns))
		}
		for _, rt := range iface.GetCompatibleResourceTypes() {
			if !isStd(rt) && !underNS(rt) {
				errs = append(errs, fmt.Errorf(
					"compatible resource_type %q is private but not under package namespace %q", rt, ns))
			}
		}
		if rt := iface.GetDefaultResourceType(); rt != "" && !isStd(rt) && !underNS(rt) {
			errs = append(errs, fmt.Errorf(
				"default resource_type %q is private but not under package namespace %q", rt, ns))
		}
	}
	for _, res := range d.GetResources() {
		rt := res.GetResourceType()
		if !isStd(rt) && !underNS(rt) {
			errs = append(errs, fmt.Errorf(
				"resource_type %q is private but not under package namespace %q", rt, ns))
		}
	}
	for _, perm := range d.GetPermissions() {
		// perm.* is the platform permission namespace. Only its wire shape is allowed through here; whether the definer
		// has Platform trust / the required signer role must be decided independently by nervud, which holds the verification results.
		// Other private permissions must still be strictly within the definer's namespace.
		if !strings.HasPrefix(perm.GetId(), "perm.") && !underNS(perm.GetId()) {
			errs = append(errs, fmt.Errorf(
				"defined permission %q not under package namespace %q", perm.GetId(), ns))
		}
	}
	return errs
}
