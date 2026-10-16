package endpoint

import (
	"context"
	"errors"
	"strings"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

// Reproduces a real incident: pkgmanagerd was started on demand, completed its handshake, was refused when reporting in because
// of a schema hash mismatch, and exited at once, while the Launcher that started the Resolve got
// RESOURCE_NOT_FOUND, a reason pointing at the selector and nothing to do with the real cause.
//
// Asserts two things:
//   - the reason no longer points at resources. The selector already resolved successfully in authorizeOnDemandStart,
//     or it would never have reached the start step, so a resource reason on this path is always misleading
//   - Resolve's own audit line carries the real refusal cause, so nobody has to match timestamps against another audit record
func TestResolveReportsRejectedRegistrationInsteadOfResourceNotFound(t *testing.T) {
	definitions := defaultCatalog(t)
	source, _ := packageManagerSource(t)
	publishSources(t, definitions, source)

	permissions := newFakePerm()
	permissions.grant(packageManagerPackage, permServiceRegister)
	permissions.grant(testCallerPackage, "perm.pkg.query")

	pkgs := newFakePkgs(serviceEntry(
		packageManagerPackage,
		packageManagerComponent,
		catalog.InterfacePackageManager,
		pkgregistry.VisibilityPublic,
	))

	aud := &fakeAudit{}
	var module *Module

	// Reports in as soon as it starts, but with a stale hash: exactly what the kernel sees when pkgmanagerd
	// forgets to fill in SchemaHash
	starter := &fakeStarter{fn: func(_ context.Context, pkg, comp string) error {
		go module.RegisterEndpoint(
			"provider",
			identity.Caller{PackageID: pkg, ComponentID: comp},
			&ipcv1.RegisterEndpoint{
				RequestId:      1,
				InterfaceId:    catalog.InterfacePackageManager,
				InterfaceMajor: packageManagerMajor,
				// Left empty = forgotten. v2 removed the compatibility bridge that let empty hashes through
			},
		)
		return nil
	}}

	module = New(definitions, pkgs, permissions, starter, aud, nil)

	result := module.ResolveEndpoint(
		"caller",
		identity.Caller{PackageID: testCallerPackage},
		&ipcv1.ResolveEndpoint{
			RequestId:         1,
			InterfaceId:       catalog.InterfacePackageManager,
			MinInterfaceMajor: packageManagerMajor,
			MaxInterfaceMajor: packageManagerMajor,
		},
	)

	if code := result.GetFailure().GetCode(); code != ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE {
		t.Errorf("code = %v, want UNAVAILABLE (the provider exists and was allowed to start, it just isn't ready)", code)
	}
	assertResolveReason(
		t, result, ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND)

	resolveErr := lastAuditErr(t, aud, "endpoint.ResolveEndpoint")
	if !errors.Is(resolveErr, errOnDemandRejected) {
		t.Fatalf("Resolve audit err = %v, want it to wrap errOnDemandRejected", resolveErr)
	}
	if !strings.Contains(resolveErr.Error(), "schema hash") {
		t.Errorf("the Resolve audit doesn't carry the real refusal cause: %v", resolveErr)
	}
	if !strings.Contains(resolveErr.Error(), packageManagerPackage) {
		t.Errorf("the Resolve audit doesn't say which component was refused: %v", resolveErr)
	}
}

// When the component never came up, it must not claim "refused": that would send people to the RegisterEndpoint audit,
// where there's nothing.
func TestResolveKeepsTimeoutWhenNothingRegistered(t *testing.T) {
	definitions := defaultCatalog(t)
	source, _ := packageManagerSource(t)
	publishSources(t, definitions, source)

	permissions := newFakePerm()
	permissions.grant(testCallerPackage, "perm.pkg.query")

	aud := &fakeAudit{}
	module := New(
		definitions,
		newFakePkgs(serviceEntry(
			packageManagerPackage,
			packageManagerComponent,
			catalog.InterfacePackageManager,
			pkgregistry.VisibilityPublic,
		)),
		permissions,
		&fakeStarter{}, // starts successfully but never reports in
		aud,
		nil,
	)

	result := module.ResolveEndpoint(
		"caller",
		identity.Caller{PackageID: testCallerPackage},
		&ipcv1.ResolveEndpoint{
			RequestId:         1,
			InterfaceId:       catalog.InterfacePackageManager,
			MinInterfaceMajor: packageManagerMajor,
			MaxInterfaceMajor: packageManagerMajor,
		},
	)

	assertResolveReason(
		t, result, ipcv1.ResolveEndpointReason_RESOLVE_ENDPOINT_REASON_INTERFACE_NOT_FOUND)
	if err := lastAuditErr(t, aud, "endpoint.ResolveEndpoint"); !errors.Is(err, errOnDemandTimeout) {
		t.Fatalf("Resolve audit err = %v, want errOnDemandTimeout", err)
	}
}

func lastAuditErr(t *testing.T, aud *fakeAudit, action string) error {
	t.Helper()
	aud.mu.Lock()
	defer aud.mu.Unlock()
	for i := len(aud.events) - 1; i >= 0; i-- {
		if aud.events[i].Action == action {
			return aud.events[i].Err
		}
	}
	t.Fatalf("no %s in the audit", action)
	return nil
}
