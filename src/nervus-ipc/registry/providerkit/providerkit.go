// Package providerkit generates the Provider contract artefacts for **pure interface-exporting** packages
// (provider.binpb + schemas.binpb), and commits them to this repository for the JVM packaging chain to use.
//
// # What gap it fills
//
// nervud's catalog builder has a hard requirement (internal/catalog/builder.go):
//
//	any non-kernel source whose manifest has exports must also provide ProviderArtifacts,
//	otherwise the whole Catalog fails to build, and that package is quarantined at the boot scan.
//
// Services written in Go each have their own providergen (pkgmanagerd / camerad) to produce these two files.
// But system apps written in Kotlin **can't produce them**: the artefacts must be Go's Deterministic protobuf
// encoding, and protobuf-java doesn't guarantee byte-for-byte equality with it. It's the same problem as stdinterface's schema
// hash table, one level more awkward: the schema hash is a platform-wide constant, while the
// descriptor is per package (containing the package_id and each interface's bar).
//
// So a Kotlin app wanting to export an interface got stuck here. permissionui implementing
// nervus.interface.permission.ui is exactly that case.
//
// This package has the Go side generate them once, written as artefacts committed to the repository, and the JVM side only copies them, never computes;
// the same shape as stdinterface (Go builds -> committed artefacts -> a gate pins them so they can't drift).
//
// # Boundary: only "pure interface exporters"
//
// That is, packages with zero ManagedResources and zero custom permissions. That covers permissionui and most future
// Kotlin system apps: what they export are software interfaces, occupying no physical devices.
//
// Something like camerad is **not** included: its resource set has to read board-level JSON, and how many cameras a board has is
// a board-level fact, not a code constant. Forcing the two together would make this package understand board configuration, while that logic
// already lives in camerad's own providergen and must read the same file as run time does.
//
// Custom permissions aren't included either: defining a permission needs localised text and three bars (GrantMode / RiskClass /
// MinimumTrust), which need reviewing one by one and shouldn't hide in a "generated in passing" table.
//
// # One place that must be right
//
// If an interface **is also defined in the kernel bootstrap** (permission.ui is), the copy this package declares
// must match the kernel's word for word: the catalog's sameInterfaceContract runs
// reflect.DeepEqual on the two definitions plus proto.Equal on every MethodMeta, and refuses the second Provider when they differ.
//
// The symptom of getting one field wrong is "two parties wrote the same interface, yet the kernel judged it a conflict", and it only surfaces
// at the boot scan on real hardware, when the image is already flashed. There's a separate contract test on the nervud side pinning this down.
package providerkit

import (
	"errors"
	"fmt"
	"path"
	"sort"

	permissionv1 "github.com/nervus-os/nervus-ipc/protocol/interface/permissionv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/nervus-os/nervus-ipc/registry"
)

// Artefact file names. They match nervud pkgregistry's ProviderArtifactsRef and the names
// nervus-system-server's providergens use; these two names are what the manifest says.
const (
	DescriptorFileName = "provider.binpb"
	SchemasFileName    = "schemas.binpb"
)

// ResourceRoot is the root directory of the committed artefacts in this repository.
//
// It's under jvm/protocol/src/main/resources so it rides nervus-app-sdk's existing
// cloneProtocol pipeline: that copies everything under jvm/protocol/src/main, so the artefacts
// ship with the SDK jar and change with the pin; there's no "the pin was updated but the binpb is still
// old" mismatch, the hardest kind to track down.
//
// It's deliberately not under java/ either: buf generate's clean deletes that whole directory (it's protoc's output),
// and hand-written or separately generated files put there would vanish at the next regeneration.
const ResourceRoot = "jvm/protocol/src/main/resources/provider"

// Interface is one exported interface declaration.
//
// The fields correspond one to one with ipcv1.ProvidedInterface, but only the ones "pure interface exporters" use are exposed:
// the three resource-related fields (compatible_resource_types / default_resource_type /
// default_resource_role) are always left empty; see the boundary notes in the package comment.
type Interface struct {
	// ID is the interface_id. Standard interfaces use nervus.*; private interfaces must be in this package's namespace
	// (registry.ValidateOEMNamespace enforces it).
	ID string

	// Major is the interface major version. 0 is reserved.
	Major uint32

	// RequiredPermission is the **interface-level** bar, i.e. "can it be Resolved at all".
	//
	// If the kernel bootstrap also defines the same interface, this must match that copy word for word;
	// it's one of the things sameInterfaceContract compares.
	//
	// Empty means no interface-level bar (method-level required_permission still applies).
	RequiredPermission string

	// MethodEnum is the enum descriptor carrying method_meta.
	//
	// It takes the generated code's enum rather than writing literals: if the enum is renamed or moved to another file, this won't compile;
	// literals would only show up at run time as a vague hash mismatch.
	MethodEnum protoreflect.EnumDescriptor

	// EventEnum is the enum descriptor carrying event_meta; nil for interfaces that push no events.
	EventEnum protoreflect.EnumDescriptor
}

// Spec is one package's Provider contract declaration.
type Spec struct {
	// PackageID must match the manifest's package_id: nervud's
	// loadRequiredProviderArtifacts compares the two and refuses to load on a mismatch.
	PackageID string

	// Interfaces are all the interfaces this package exports.
	//
	// **Every one must also appear in the exports of some component in the manifest**:
	// the catalog's addArtifacts closes in both directions; something in the descriptor but not in exports,
	// or the reverse, is refused either way.
	Interfaces []Interface
}

// All returns every package with committed artefacts, in a stable order.
//
// New packages always go at the end. This list and nervud internal/catalog's bootstrap are two
// independent sources of truth, so **interfaces the kernel also defines** must match word for word; there's a
// contract test on the nervud side pinning it (see the package comment).
//
// permission.ui's three values come from that bootstrapInterface call in nervud's bootstrap:
//
//	required_permission = "perm.pkg.query"   the interface-level bar, i.e. "can it be Resolved at all"
//	risk_class          = UNSPECIFIED        binds no physical resource
//	the three resource fields = empty
//
// Note especially that required_permission is **not** perm.permission.admin: that's the bar of
// permission.admin (the kernel's built-in grant surface). This interface is "ask for a confirmation to be shown to the user",
// called by Settings and the desktop, which hold perm.pkg.query and shouldn't hold the key to the grant surface.
func All() []Spec {
	return []Spec{
		{
			PackageID: "nervus.permissionui",
			Interfaces: []Interface{{
				ID:                 "nervus.interface.permission.ui",
				Major:              1,
				RequiredPermission: "perm.pkg.query",
				MethodEnum:         permissionv1.PermissionUiMethod(0).Descriptor(),
			}},
		},
	}
}

// Artifacts are the two deterministic byte strings generated for one package.
type Artifacts struct {
	PackageID      string
	DescriptorWire []byte
	SchemaWire     []byte
}

// DescriptorPath and SchemasPath return the artefacts' relative paths in the repository.
func (a Artifacts) DescriptorPath() string {
	return path.Join(ResourceRoot, a.PackageID, DescriptorFileName)
}

func (a Artifacts) SchemasPath() string {
	return path.Join(ResourceRoot, a.PackageID, SchemasFileName)
}

// Build generates the two deterministic protobuf byte strings from a spec.
//
// It goes through registry.BuildSchemaBundleWithEvents and registry.MarshalProviderArtifacts,
// **the same constructors** as nervud's bootstrap and every providergen. That's the only guarantee that the several parties
// don't drift: any side switching to another algorithm shows up immediately as a hash mismatch or contract conflict.
//
// MarshalProviderArtifacts parses its output back right after encoding to check it, so this function
// returning success means the two byte strings pass the same checks as kernel loading; caught at packaging time,
// better than finding out at the target machine's scan, when the image is already flashed.
func Build(spec Spec) (Artifacts, error) {
	if spec.PackageID == "" {
		return Artifacts{}, errors.New("providerkit: empty package id")
	}
	if len(spec.Interfaces) == 0 {
		// A package that exports no interfaces doesn't need ProviderArtifacts at all. Generating an empty one
		// would only add a provider section to the manifest, and the kernel would refuse to load it for reasons like "the descriptor has no
		// interfaces", an error far from "you don't actually need it".
		return Artifacts{}, fmt.Errorf(
			"providerkit: package %q declares no interfaces; omit the provider section instead",
			spec.PackageID)
	}

	descriptor := &ipcv1.ProviderDescriptor{
		PackageId:  spec.PackageID,
		Interfaces: make([]*ipcv1.ProvidedInterface, 0, len(spec.Interfaces)),
		// No Resources or Permissions declared: see the boundary notes in the package comment
	}
	bundles := &ipcv1.InterfaceSchemaBundleSet{
		Bundles: make([]*ipcv1.InterfaceSchemaBundle, 0, len(spec.Interfaces)),
	}

	seen := make(map[string]struct{}, len(spec.Interfaces))
	for _, iface := range spec.Interfaces {
		if iface.ID == "" {
			return Artifacts{}, fmt.Errorf("providerkit: package %q has an interface with empty id",
				spec.PackageID)
		}
		// The same interface_id twice would put two conflicting records in the descriptor, and which one a reader
		// gets would depend on how it iterates. The registry's checks would refuse it too, but reporting it here
		// points straight at that line of the spec.
		if _, dup := seen[iface.ID]; dup {
			return Artifacts{}, fmt.Errorf("providerkit: package %q declares interface %q twice",
				spec.PackageID, iface.ID)
		}
		seen[iface.ID] = struct{}{}

		bundle, err := registry.BuildSchemaBundleWithEvents(
			iface.ID, iface.Major, iface.MethodEnum, iface.EventEnum)
		if err != nil {
			return Artifacts{}, fmt.Errorf("providerkit: package %q interface %q: %w",
				spec.PackageID, iface.ID, err)
		}
		bundles.Bundles = append(bundles.Bundles, bundle)

		descriptor.Interfaces = append(descriptor.Interfaces, &ipcv1.ProvidedInterface{
			InterfaceId: iface.ID,
			InterfaceVersions: []*ipcv1.ProvidedInterfaceVersion{{
				Major: iface.Major,
				// schema_hash takes the bundle's hash, not MethodsHash: an empty methods field
				// means this is **not** a metadata interface; it has a full descriptor set,
				// so request/response types can be resolved and checked.
				SchemaHash: append([]byte(nil), bundle.GetSchemaHash()...),
			}},
			RequiredPermission: iface.RequiredPermission,
			// The three resource fields and the risk floor are all left zero: pure interface exporters bind no physical resource.
			// Note resource_risk_floor must be UNSPECIFIED: the registry's checks
			// require "declaring compatible_resource_types needs a risk floor",
			// and conversely leaving it UNSPECIFIED without resources is right.
		})
	}

	// Both slices are written sorted by interface_id.
	//
	// Deterministic encoding only guarantees field order, and doesn't sort repeated elements for us;
	// that means reordering the spec would produce artefacts with equivalent content but different bytes,
	// so the digest would change and the gate would go red, without anything really changing.
	sort.Slice(descriptor.Interfaces, func(i, j int) bool {
		return descriptor.Interfaces[i].GetInterfaceId() < descriptor.Interfaces[j].GetInterfaceId()
	})
	sort.Slice(bundles.Bundles, func(i, j int) bool {
		return bundles.Bundles[i].GetInterfaceId() < bundles.Bundles[j].GetInterfaceId()
	})

	descriptorWire, schemaWire, err := registry.MarshalProviderArtifacts(descriptor, bundles)
	if err != nil {
		return Artifacts{}, fmt.Errorf("providerkit: package %q: %w", spec.PackageID, err)
	}
	return Artifacts{
		PackageID:      spec.PackageID,
		DescriptorWire: descriptorWire,
		SchemaWire:     schemaWire,
	}, nil
}
