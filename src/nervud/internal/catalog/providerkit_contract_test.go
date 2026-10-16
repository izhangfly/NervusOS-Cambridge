package catalog

import (
	"testing"

	ipcregistry "github.com/nervus-os/nervus-ipc/registry"
	"github.com/nervus-os/nervus-ipc/registry/providerkit"

	"github.com/nervus-os/nervud/internal/identity"
)

// This file pins down something that only shows up on real hardware: the ProviderArtifacts nervus-ipc's providerkit
// generates for Kotlin system apps must match the definitions of the same-named interfaces in this package's bootstrap
// word for word.
//
// # Why it needs a dedicated test
//
// permissionui exports nervus.interface.permission.ui, and the kernel bootstrap defines
// the same interface too (it's a standard interface). When the two definitions meet, the Builder takes sameInterfaceContract:
// reflect.DeepEqual on the InterfaceDefinition, then proto.Equal on every MethodMeta and EventMeta.
// Any difference (one character off in required_permission, one extra tier in the risk floor)
// makes the second Provider be refused.
//
// That failure is not caught at compile time, nor by either repository's own tests: providerkit is in nervus-ipc,
// bootstrap is here, and each side passes its own tests independently. The symptom is that an image with permissionui
// installed quarantines it at first boot, with an error message about an "interface conflict", far from the real cause
// ("the two required_permission values differ"), and by then the image has already been flashed.
//
// So the two definitions are crashed into each other here. This pin matters more than the artifacts themselves.

// providerkitSpecFor takes a package's spec from providerkit's central list.
//
// Deliberately no rewritten spec in this file: then the test would become "the copy I made matches bootstrap",
// while what really goes into the image is providerkit's.
func providerkitSpecFor(t *testing.T, packageID string) providerkit.Spec {
	t.Helper()
	for _, spec := range providerkit.All() {
		if spec.PackageID == packageID {
			return spec
		}
	}
	t.Fatalf("providerkit.All() has no spec for %q", packageID)
	return providerkit.Spec{}
}

// providerkitArtifacts generates artifacts with providerkit.Build and parses them into the form the Catalog accepts.
//
// Build rather than reading the committed .binpb files: those two files are in the nervus-ipc repository,
// and this repository's tests shouldn't depend on another repository's layout (paths in the go module cache aren't stable).
// providerkit's own gate already pins "committed bytes == Build output", so testing
// Build's output here is equivalent to testing those two files.
func providerkitArtifacts(t *testing.T, packageID string) *ipcregistry.ProviderArtifacts {
	t.Helper()
	built, err := providerkit.Build(providerkitSpecFor(t, packageID))
	if err != nil {
		t.Fatalf("providerkit.Build(%q): %v", packageID, err)
	}
	artifacts, err := ipcregistry.ParseProviderArtifacts(built.DescriptorWire, built.SchemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts(%q): %v", packageID, err)
	}
	return artifacts
}

// permissionUISource builds a Source equivalent to the real permissionui package.
//
// Trust and Signers must be platform-release: authorizeNewInterface requires that role for interfaces with the
// nervus.* prefix (see builder.go). The signing.role in permissionui's nspkg
// really is platform-release, and this reflects it faithfully.
func permissionUISource(t *testing.T) Source {
	t.Helper()
	return Source{
		PackageID: "nervus.permissionui",
		Kind:      SourceKindSystemImage,
		Trust:     identity.TrustPlatform,
		Signers: SignerEvidence{
			Roles: []string{rolePlatformRelease},
			VerifiedSigners: []VerifiedSigner{{
				Role: rolePlatformRelease, KeyID: "platform-release-key",
			}},
		},
		// Exports must correspond one to one with the interfaces declared in the descriptor: addArtifacts
		// closes in both directions. Something in the descriptor but not in exports, or the reverse, is refused either way.
		Exports: []ExportBinding{{
			ComponentID: "main",
			InterfaceID: InterfacePermissionUI,
		}},
		Artifacts: providerkitArtifacts(t, "nervus.permissionui"),
	}
}

// TestProviderkitPermissionUIMatchesBootstrap is the core gate.
//
// Prepare succeeding means providerkit's definition passed sameInterfaceContract.
// That function is private, but Prepare is its only entry point, so a contract mismatch returns an error here.
func TestProviderkitPermissionUIMatchesBootstrap(t *testing.T) {
	registry := mustDefaultRegistry(t)
	candidate, err := registry.Prepare([]Source{permissionUISource(t)})
	if err != nil {
		t.Fatalf("providerkit artifacts conflict with bootstrap contract: %v", err)
	}

	// The interface-level bar must still be the one bootstrap declares. Prepare succeeding alone isn't enough:
	// if someone later changes both sides at the same time to another value, the contract is still self-consistent, but permission.ui's
	// visibility has quietly changed. The expected value is hard-coded here so such a change must go through this test explicitly.
	iface, ok := candidate.Snapshot().Interface(InterfacePermissionUI, 1)
	if !ok {
		t.Fatal("permission.ui interface missing from prepared snapshot")
	}
	if iface.RequiredPermission != "perm.pkg.query" {
		t.Errorf("permission.ui required permission = %q, want perm.pkg.query",
			iface.RequiredPermission)
	}

	// The request type of ConfirmInstall (method 1): pins "the schema bundle really got into the Catalog",
	// not just that a hash matched. An empty descriptor set can also produce a hash,
	// but then the method's request/response types are nil and the payload can't be checked on the call path.
	method, ok := candidate.Snapshot().ProviderMethod(
		"nervus.permissionui", InterfacePermissionUI, 1, 1)
	if !ok {
		t.Fatal("permission.ui method 1 missing from prepared snapshot")
	}
	if method.Request == nil ||
		string(method.Request.FullName()) != "nervus.interface.permission.v1.ConfirmInstallRequest" {
		t.Errorf("permission.ui method 1 request = %v, want ConfirmInstallRequest", method.Request)
	}
}

// TestProviderkitPermissionUIWrongPermissionConflicts pins, in reverse, that the gate really works.
//
// Without it, the test above couldn't tell "the contracts match" from "sameInterfaceContract was never called".
// Change required_permission to another valid permission and Prepare must fail.
func TestProviderkitPermissionUIWrongPermissionConflicts(t *testing.T) {
	spec := providerkitSpecFor(t, "nervus.permissionui")
	// Copy it and change the copy; don't touch the one providerkit.All() returns
	tampered := providerkit.Spec{
		PackageID:  spec.PackageID,
		Interfaces: make([]providerkit.Interface, len(spec.Interfaces)),
	}
	copy(tampered.Interfaces, spec.Interfaces)
	for i := range tampered.Interfaces {
		if tampered.Interfaces[i].ID == InterfacePermissionUI {
			// perm.pkg.install is a real permission, so the failure can only be the contract mismatch,
			// not "references an undefined permission"
			tampered.Interfaces[i].RequiredPermission = "perm.pkg.install"
		}
	}

	built, err := providerkit.Build(tampered)
	if err != nil {
		t.Fatalf("providerkit.Build(tampered): %v", err)
	}
	artifacts, err := ipcregistry.ParseProviderArtifacts(built.DescriptorWire, built.SchemaWire)
	if err != nil {
		t.Fatalf("ParseProviderArtifacts(tampered): %v", err)
	}

	source := permissionUISource(t)
	source.Artifacts = artifacts

	registry := mustDefaultRegistry(t)
	if _, err := registry.Prepare([]Source{source}); err == nil {
		t.Fatal("unexpected catalog result; tampered required_permission must conflict with bootstrap")
	}
}
