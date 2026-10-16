package pkgregistry

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/permission"
)

func newDevKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate dev key: %v", err)
	}
	return priv
}

func signManifest(t *testing.T, priv ed25519.PrivateKey, manifestBytes []byte) []byte {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	msg := append(append([]byte{}, manifestSigDomain...), manifestBytes...)
	sig := ed25519.Sign(priv, msg)
	sb := SignatureBlock{
		Format: 1,
		Signatures: []Signature{{
			Role: RoleDeveloper, Alg: SigAlgEd25519,
			KeyID: keyIDOf(pub),
			Key:   base64.StdEncoding.EncodeToString(pub),
			Sig:   base64.StdEncoding.EncodeToString(sig),
		}},
	}
	data, err := json.Marshal(sb)
	if err != nil {
		t.Fatalf("marshal sig block: %v", err)
	}
	return data
}

func testABI() string {
	if tok := hostABIToken(); tok != "" {
		return tok
	}
	return ABILinuxX86_64
}

type fakeInstaller struct {
	installErr    error
	dataDirErr    error
	removeErr     error
	appUserErr    error
	userAccessErr error
	installed     []authority.InstallVerifiedPackageRequest
	dataDirs      []authority.CreateDataDirRequest
	removed       []authority.RemovePackageTreeRequest
	appUsers      []authority.EnsureAppUserRequest
	userAccess    []authority.SetUserDataAccessRequest
}

// Uninstall must also remove the ACL entry on the user documents area, otherwise UID reuse would give write access to a new package for free
func (f *fakeInstaller) SetUserDataAccess(
	_ context.Context, _ authority.Subject, req authority.SetUserDataAccessRequest,
) error {
	f.userAccess = append(f.userAccess, req)
	return f.userAccessErr
}

func (f *fakeInstaller) EnsureAppUser(
	_ context.Context, _ authority.Subject, req authority.EnsureAppUserRequest,
) error {
	f.appUsers = append(f.appUsers, req)
	return f.appUserErr
}

func (f *fakeInstaller) InstallVerifiedPackage(
	_ context.Context, _ authority.Subject, req authority.InstallVerifiedPackageRequest,
) error {
	f.installed = append(f.installed, req)
	return f.installErr
}

func (f *fakeInstaller) CreatePrivateDataDirectory(
	_ context.Context, _ authority.Subject, req authority.CreateDataDirRequest,
) (authority.DirHandle, error) {
	f.dataDirs = append(f.dataDirs, req)
	if f.dataDirErr != nil {
		return authority.DirHandle{}, f.dataDirErr
	}
	return authority.DirHandle{Path: req.Path}, nil
}

func (f *fakeInstaller) RemovePackageTree(
	_ context.Context, _ authority.Subject, req authority.RemovePackageTreeRequest,
) error {
	f.removed = append(f.removed, req)
	return f.removeErr
}

type fakeIdentityUpdater struct {
	replaced [][]identity.Package
}

func (f *fakeIdentityUpdater) Replace(pkgs []identity.Package) error {
	f.replaced = append(f.replaced, pkgs)
	return nil
}

type fakeAuditor struct{ events []audit.Event }

func (f *fakeAuditor) Record(_ context.Context, ev audit.Event) { f.events = append(f.events, ev) }

type fakePermissionArbiter struct {
	intersect   func(requested []string, trust identity.TrustProfile, signerRoles []string) (granted, denied []string)
	intersectAt func(
		definitions *catalog.Snapshot,
		requested []string,
		source catalog.SourceKind,
		trust identity.TrustProfile,
		signers catalog.SignerEvidence,
	) (granted, denied []string)
	replaced [][]permission.Grant
	cleared  []string
	clearErr error

	// runtimeStates records every SetRuntimeState, for assertions about install-time consent
	runtimeStates []fakeRuntimeGrant
	runtimeErr    error

	// existingStates is the return value of GrantStateOf, keyed "pkg\x00perm".
	// Missing (zero-value map / unregistered) means NotRequested, consistent with the real Registry:
	// a permission never decided on is NOT_REQUESTED
	existingStates map[string]permission.GrantState
}

// fakeRuntimeGrant records one SetRuntimeState call
type fakeRuntimeGrant struct {
	pkg   string
	perm  string
	state permission.GrantState
}

func (f *fakePermissionArbiter) IntersectAt(
	definitions *catalog.Snapshot,
	requested []string,
	source catalog.SourceKind,
	trust identity.TrustProfile,
	signers catalog.SignerEvidence,
) (granted, denied []string) {
	if f.intersectAt != nil {
		return f.intersectAt(definitions, requested, source, trust, signers)
	}
	if f.intersect != nil {
		return f.intersect(requested, trust, signers.Roles)
	}
	return requested, nil
}

func (f *fakePermissionArbiter) Replace(grants []permission.Grant) error {
	f.replaced = append(f.replaced, grants)
	return nil
}

func (f *fakePermissionArbiter) ClearPackage(pkg string) error {
	f.cleared = append(f.cleared, pkg)
	return f.clearErr
}

func (f *fakePermissionArbiter) SetRuntimeState(
	pkg, permissionID string, state permission.GrantState,
) error {
	f.runtimeStates = append(f.runtimeStates,
		fakeRuntimeGrant{pkg: pkg, perm: permissionID, state: state})
	return f.runtimeErr
}

func (f *fakePermissionArbiter) GrantStateOf(pkg, permissionID string) permission.GrantState {
	return f.existingStates[pkg+"\x00"+permissionID]
}

// setExistingState presets an existing runtime decision, for the "upgrades don't overwrite the user's decision" assertion.
func (f *fakePermissionArbiter) setExistingState(
	pkg, permissionID string, state permission.GrantState,
) {
	if f.existingStates == nil {
		f.existingStates = make(map[string]permission.GrantState)
	}
	f.existingStates[pkg+"\x00"+permissionID] = state
}

func newTestInstaller(t *testing.T) (*Module, *fakeInstaller, *fakeIdentityUpdater, *fakeAuditor) {
	t.Helper()
	mod, auth, idReg, aud, _ := newTestInstallerWithPerm(t)
	return mod, auth, idReg, aud
}

func newTestInstallerWithPerm(t *testing.T) (*Module, *fakeInstaller, *fakeIdentityUpdater, *fakeAuditor, *fakePermissionArbiter) {
	t.Helper()
	dir := t.TempDir()
	auth := &fakeInstaller{}
	idReg := &fakeIdentityUpdater{}
	perm := &fakePermissionArbiter{}
	aud := &fakeAuditor{}
	registry := NewRegistry()
	definitions, err := catalog.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	mod := New(auth, idReg, perm, registry, definitions, TrustStore{}, aud, nil,
		filepath.Join(dir, "registry"), filepath.Join(dir, "system-packages"),
		filepath.Join(dir, "packages"), filepath.Join(dir, "data"))
	return mod, auth, idReg, aud, perm
}

func newValidStaging(t *testing.T, root, packageID, version string) (string, []byte, []byte) {
	t.Helper()
	return newValidStagingWithKey(t, root, packageID, version, 100, newDevKey(t))
}

func newValidStagingWithKey(
	t *testing.T, root, packageID, version string, versionCode uint64, priv ed25519.PrivateKey,
) (string, []byte, []byte) {
	t.Helper()
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	content := "#!/bin/true"
	if err := os.WriteFile(filepath.Join(staging, "bin"), []byte(content), 0o755); err != nil {
		t.Fatalf("write staging file: %v", err)
	}
	manifest := fmt.Sprintf(`{"schema":1,"package_id":%q,"version":%q,"version_code":%d,`+
		`"min_nervus_api":1,"target_nervus_api":1,"supported_abis":[%q],`+
		`"digests":{"bin":%q},`+
		`"components":[{"id":"main","type":"app","entry":"bin","runtime":"native","launch_mode":"manual"}]}`,
		packageID, version, versionCode, testABI(), hashOf(content))
	mb := []byte(manifest)
	sig := signManifest(t, priv, mb)
	writeStagingMetadata(t, staging, mb, sig)
	return staging, mb, sig
}

func writeStagingMetadata(t *testing.T, staging string, manifestBytes, sig []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(staging, ManifestFileName), manifestBytes, 0o644); err != nil {
		t.Fatalf("write staging manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, SignatureFileName), sig, 0o644); err != nil {
		t.Fatalf("write staging sig: %v", err)
	}
}

// The three kernel bootstrap permissions the install-time consent tests use, each representing one criterion:
//
//	permStorageUser   USER_CONSENT: has runtime state, the only tier consent can act on
//	permPkgQuery      NORMAL: takes effect on install, no runtime state
//	permMotionControl USER_CONSENT: but the test doesn't request it, to check "not in the grant set"
const (
	permStorageUser   = "perm.storage.user"
	permPkgQuery      = "perm.pkg.query"
	permMotionControl = "perm.motion.control"
)

// newStagingWithPermissions makes a staging directory that declares permissions.
//
// fakePermissionArbiter's default IntersectAt approves every request unchanged, so requested
// is the final GrantedPermissions, which is what we want: this group of tests cares how the consent list gets
// trimmed, not the install-time decision itself
func newStagingWithPermissions(
	t *testing.T, root, packageID string, permissions []string,
) (string, []byte, []byte) {
	t.Helper()
	staging := filepath.Join(root, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	content := "#!/bin/true"
	if err := os.WriteFile(filepath.Join(staging, "bin"), []byte(content), 0o755); err != nil {
		t.Fatalf("write staging file: %v", err)
	}
	quoted := make([]string, 0, len(permissions))
	for _, p := range permissions {
		quoted = append(quoted, fmt.Sprintf("%q", p))
	}
	manifest := fmt.Sprintf(`{"schema":1,"package_id":%q,"version":"1.0.0","version_code":1,`+
		`"min_nervus_api":1,"target_nervus_api":1,"supported_abis":[%q],`+
		`"digests":{"bin":%q},"permissions":[%s],`+
		`"components":[{"id":"main","type":"app","entry":"bin","runtime":"native","launch_mode":"manual"}]}`,
		packageID, testABI(), hashOf(content), strings.Join(quoted, ","))
	mb := []byte(manifest)
	sig := signManifest(t, newDevKey(t), mb)
	writeStagingMetadata(t, staging, mb, sig)
	return staging, mb, sig
}

// TestInstallDefaultGrant_OnlyUserConsentInsideInstallSet pins the trimming criterion of default grants.
//
// From V2 on, grants happen on install (see the notes on applyInstallConsent), but the criterion for "what to grant" isn't loosened:
// only those that are both "in the install-time grant set" and "in the USER_CONSENT tier" are stored.
//
//	NORMAL permissions        take effect on install and have no runtime state. Writing a state for one
//	                          only gets an error back from SetRuntimeState
//	permissions never asked for   not in GrantedPermissions, and a default grant can't conjure them up;
//	                          otherwise any package could get capabilities it never requested
func TestInstallDefaultGrant_OnlyUserConsentInsideInstallSet(t *testing.T) {
	mod, _, _, _, perm := newTestInstallerWithPerm(t)
	root := t.TempDir()

	// Only these two are requested: one USER_CONSENT, one NORMAL.
	// permMotionControl is deliberately not requested, to check "not in the grant set means not granted"
	staging, manifestBytes, sig := newStagingWithPermissions(
		t, root, "com.example.consent", []string{permStorageUser, permPkgQuery})

	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes,
		SigBlock:      sig,
		StagingDir:    staging,
		Source:        SourceDynamicInstall,
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(perm.runtimeStates) != 1 {
		t.Fatalf("SetRuntimeState calls = %+v, want exactly 1", perm.runtimeStates)
	}
	got := perm.runtimeStates[0]
	if got.pkg != "com.example.consent" || got.perm != permStorageUser {
		t.Fatalf("granted %s/%s, want com.example.consent/%s", got.pkg, got.perm, permStorageUser)
	}
	if got.state != permission.GrantStateGranted {
		t.Fatalf("state = %v, want Granted", got.state)
	}
}

// TestInstallDefaultGrant_KeepsUserDecision pins "upgrades don't overwrite the user's existing decisions".
//
// Only uninstall clears _grants.json (ClearPackage is only called on the uninstall path); upgrades don't. So on reinstall
// the previous decisions are still there, and a permission the user explicitly switched off must never be switched back on by a version update;
// otherwise every app would have a way round the user's decision: ship a new version and get the permission back.
func TestInstallDefaultGrant_KeepsUserDecision(t *testing.T) {
	mod, _, _, _, perm := newTestInstallerWithPerm(t)
	root := t.TempDir()

	// The user had explicitly switched this permission off before
	perm.setExistingState("com.example.revoked", permStorageUser, permission.GrantStateDenied)

	staging, manifestBytes, sig := newStagingWithPermissions(
		t, root, "com.example.revoked", []string{permStorageUser})

	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes,
		SigBlock:      sig,
		StagingDir:    staging,
		Source:        SourceDynamicInstall,
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(perm.runtimeStates) != 0 {
		t.Fatalf("unexpected SetRuntimeState calls = %+v; a revoked permission must not "+
			"be re-granted by an upgrade", perm.runtimeStates)
	}
}

// TestInstallDefaultGrant_NoPermissionsGrantsNothing: installing a package that requests no permissions
// shouldn't produce any runtime grant writes either.
func TestInstallDefaultGrant_NoPermissionsGrantsNothing(t *testing.T) {
	mod, _, _, _, perm := newTestInstallerWithPerm(t)
	root := t.TempDir()
	staging, manifestBytes, sig := newStagingWithPermissions(
		t, root, "com.example.noperms", nil)

	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes,
		SigBlock:      sig,
		StagingDir:    staging,
		Source:        SourceDynamicInstall,
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(perm.runtimeStates) != 0 {
		t.Fatalf("SetRuntimeState calls = %+v, want none", perm.runtimeStates)
	}
}

func TestInstall_Success(t *testing.T) {
	mod, auth, idReg, aud := newTestInstaller(t)
	root := t.TempDir()
	staging, manifestBytes, sig := newValidStaging(t, root, "com.example.app", "1.0.0")

	entry, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes,
		SigBlock:      sig,
		StagingDir:    staging,
		Source:        SourceDynamicInstall,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if entry.Manifest.PackageID != "com.example.app" || entry.ActiveVersion != "1.0.0" {
		t.Fatalf("got entry %+v", entry)
	}
	if entry.Trust != identity.TrustOrdinary {
		t.Fatalf("Trust = %v, want TrustOrdinary", entry.Trust)
	}

	if len(auth.installed) != 1 {
		t.Fatalf("want exactly one InstallVerifiedPackage call, got %d", len(auth.installed))
	}
	if len(auth.dataDirs) != 1 {
		t.Fatalf("want exactly one CreatePrivateDataDirectory call, got %d", len(auth.dataDirs))
	}
	if len(idReg.replaced) != 1 || len(idReg.replaced[0]) != 1 {
		t.Fatalf("identity projection not pushed correctly: %+v", idReg.replaced)
	}
	if mod.registry.Len() != 1 {
		t.Fatalf("Registry.Len() = %d, want 1", mod.registry.Len())
	}

	found := false
	for _, ev := range aud.events {
		if ev.Action == "pkgregistry.Install" && !ev.Denied {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a successful pkgregistry.Install audit event, got %+v", aud.events)
	}
}

func TestInstall_RejectsOnDigestMismatch(t *testing.T) {
	mod, auth, idReg, _ := newTestInstaller(t)
	root := t.TempDir()
	staging, manifestBytes, sig := newValidStaging(t, root, "com.example.app", "1.0.0")

	if err := os.WriteFile(filepath.Join(staging, "bin"), []byte("tampered"), 0o755); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes, SigBlock: sig, StagingDir: staging, Source: SourceDynamicInstall,
	})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	if len(auth.installed) != 0 || len(auth.dataDirs) != 0 {
		t.Fatalf("Authority should not be called: installed=%d dataDirs=%d", len(auth.installed), len(auth.dataDirs))
	}
	if len(idReg.replaced) != 0 {
		t.Fatal("identity should not be updated")
	}
	if mod.registry.Len() != 0 {
		t.Fatal("Registry should not be updated")
	}
}

func TestInstall_RejectsMalformedManifest(t *testing.T) {
	mod, auth, _, _ := newTestInstaller(t)
	_, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: []byte(`{"schema":1,"package_id":""}`),
		StagingDir:    t.TempDir(),
		Source:        SourceDynamicInstall,
	})
	if !errors.Is(err, ErrEmptyPackageID) {
		t.Fatalf("err = %v, want ErrEmptyPackageID", err)
	}
	if len(auth.installed) != 0 {
		t.Fatal("Authority should not be called")
	}
}

func TestInstall_PropagatesAuthorityFailure(t *testing.T) {
	mod, auth, idReg, aud := newTestInstaller(t)
	auth.installErr = errors.New("boom: disk full")

	root := t.TempDir()
	staging, manifestBytes, sig := newValidStaging(t, root, "com.example.app", "1.0.0")

	_, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes, SigBlock: sig, StagingDir: staging, Source: SourceDynamicInstall,
	})
	if err == nil {
		t.Fatal("want error")
	}
	if mod.registry.Len() != 0 {
		t.Fatal("Registry should not be committed after an Authority failure")
	}
	if len(idReg.replaced) != 0 {
		t.Fatal("identity should not be updated after an Authority failure")
	}

	found := false
	for _, ev := range aud.events {
		if ev.Action == "pkgregistry.Install" && ev.Denied {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a denied pkgregistry.Install audit event, got %+v", aud.events)
	}
}

func TestInstall_ComputesGrantedPermissions(t *testing.T) {
	mod, _, _, aud, perm := newTestInstallerWithPerm(t)
	perm.intersect = func(requested []string, _ identity.TrustProfile, _ []string) (granted, denied []string) {
		return []string{"perm.granted"}, []string{"perm.denied"}
	}

	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	content := "#!/bin/true"
	if err := os.WriteFile(filepath.Join(staging, "bin"), []byte(content), 0o755); err != nil {
		t.Fatalf("write staging file: %v", err)
	}
	manifestBytes := []byte(fmt.Sprintf(`{"schema":1,"package_id":"com.example.app","version":"1.0.0",`+
		`"version_code":100,"min_nervus_api":1,"target_nervus_api":1,"supported_abis":[%q],`+
		`"digests":{"bin":%q},`+
		`"permissions":["perm.granted","perm.denied"],`+
		`"components":[{"id":"main","type":"app","entry":"bin","runtime":"native","launch_mode":"manual"}]}`,
		testABI(), hashOf(content)))
	sig := signManifest(t, newDevKey(t), manifestBytes)
	writeStagingMetadata(t, staging, manifestBytes, sig)

	entry, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes, SigBlock: sig, StagingDir: staging, Source: SourceDynamicInstall,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(entry.GrantedPermissions) != 1 || entry.GrantedPermissions[0] != "perm.granted" {
		t.Fatalf("GrantedPermissions = %v, want [perm.granted]", entry.GrantedPermissions)
	}

	found := false
	for _, ev := range aud.events {
		if ev.Action == "pkgregistry.IntersectAt" && ev.Denied {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a denied pkgregistry.IntersectAt audit event, got %+v", aud.events)
	}

	if len(perm.replaced) != 1 || len(perm.replaced[0]) != 1 {
		t.Fatalf("permission projection was not published correctly: %+v", perm.replaced)
	}
	if got := perm.replaced[0][0]; got.PackageID != "com.example.app" || len(got.Permissions) != 1 {
		t.Fatalf("projection content = %+v", got)
	}
}

func TestInstall_UpgradeReplacesOldVersion(t *testing.T) {
	mod, _, _, _ := newTestInstaller(t)
	key := newDevKey(t)

	root1 := t.TempDir()
	staging1, manifest1, sig1 := newValidStagingWithKey(t, root1, "com.example.app", "1.0.0", 100, key)
	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifest1, SigBlock: sig1, StagingDir: staging1, Source: SourceDynamicInstall,
	}); err != nil {
		t.Fatalf("install v1: %v", err)
	}

	root2 := t.TempDir()
	staging2, manifest2, sig2 := newValidStagingWithKey(t, root2, "com.example.app", "2.0.0", 200, key)
	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifest2, SigBlock: sig2, StagingDir: staging2, Source: SourceDynamicInstall,
	}); err != nil {
		t.Fatalf("install v2: %v", err)
	}

	if mod.registry.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 because an upgrade should replace rather than accumulate", mod.registry.Len())
	}
	e, ok := mod.registry.Lookup("com.example.app")
	if !ok || e.ActiveVersion != "2.0.0" || e.VersionCode != 200 {
		t.Fatalf("got %+v, want active version 2.0.0 code 200", e)
	}
}

func newProvisionModule(t *testing.T, auth *fakeInstaller) *Module {
	t.Helper()
	dir := t.TempDir()
	definitions, err := catalog.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	return New(auth, &fakeIdentityUpdater{}, &fakePermissionArbiter{}, NewRegistry(),
		definitions, TrustStore{}, &fakeAuditor{}, nil,
		filepath.Join(dir, "registry"), filepath.Join(dir, "system-packages"),
		filepath.Join(dir, "packages"), filepath.Join(dir, "data"))
}

//

//

func TestInstall_EnsuresAppUser(t *testing.T) {
	mod, auth, _, _ := newTestInstaller(t)
	root := t.TempDir()
	staging, manifestBytes, sig := newValidStaging(t, root, "com.example.app", "1.0.0")

	entry, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes,
		SigBlock:      sig,
		StagingDir:    staging,
		Source:        SourceDynamicInstall,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(auth.appUsers) != 1 {
		t.Fatalf("unexpected package registry result; EnsureAppUser %d, want 1", len(auth.appUsers))
	}
	u := auth.appUsers[0]
	if u.UID != entry.UID || u.GID != entry.UID {
		t.Errorf("unexpected package registry result; uid/gid = %d/%d, want %d/%d GID UID", u.UID, u.GID, entry.UID, entry.UID)
	}
	if u.Name != authority.AppUserName(entry.UID) {
		t.Errorf("name = %q, want %q", u.Name, authority.AppUserName(entry.UID))
	}
}

//

func TestInstall_EnsuresAppUserOnUpgradeToo(t *testing.T) {
	mod, auth, _, _ := newTestInstaller(t)
	root := t.TempDir()

	key := newDevKey(t)
	for i, ver := range []string{"1.0.0", "1.0.1"} {
		staging, manifestBytes, sig := newValidStagingWithKey(
			t, root, "com.example.app", ver, uint64(100+i), key)
		if _, err := mod.Install(context.Background(), InstallTransaction{
			ManifestBytes: manifestBytes,
			SigBlock:      sig,
			StagingDir:    staging,
			Source:        SourceDynamicInstall,
		}); err != nil {
			t.Fatalf("Install %s: %v", ver, err)
		}
	}

	if len(auth.appUsers) != 2 {
		t.Errorf("unexpected package registry result; EnsureAppUser %d, want 2", len(auth.appUsers))
	}

	if len(auth.dataDirs) != 1 {
		t.Errorf("unexpected package registry result; CreatePrivateDataDirectory %d, want 1", len(auth.dataDirs))
	}
}

//

func TestInstall_SameVersionAsksToReplace(t *testing.T) {
	mod, auth, _, _ := newTestInstaller(t)
	root := t.TempDir()
	key := newDevKey(t)

	staging, mb, sig := newValidStagingWithKey(t, root, "com.example.app", "1.0.0", 100, key)
	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: mb, SigBlock: sig, StagingDir: staging, Source: SourceDynamicInstall,
	}); err != nil {
		t.Fatalf("unexpected package registry result; value = %v", err)
	}
	if auth.installed[0].ReplaceExisting {
		t.Error("unexpected package registry result; ReplaceExisting")
	}

	staging2, mb2, sig2 := newValidStagingWithKey(t, root, "com.example.app", "1.0.0", 100, key)
	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: mb2, SigBlock: sig2, StagingDir: staging2, Source: SourceDynamicInstall,
	}); err != nil {
		t.Fatalf("unexpected package registry result; value = %v", err)
	}
	if len(auth.installed) != 2 {
		t.Fatalf("unexpected package registry result; InstallVerifiedPackage %d, want 2", len(auth.installed))
	}
	if !auth.installed[1].ReplaceExisting {
		t.Error("unexpected package registry result; ReplaceExisting renameat2 EEXIST")
	}
}

func TestInstall_UpgradeDoesNotAskToReplace(t *testing.T) {
	mod, auth, _, _ := newTestInstaller(t)
	root := t.TempDir()
	key := newDevKey(t)

	for i, ver := range []string{"1.0.0", "1.0.1"} {
		staging, mb, sig := newValidStagingWithKey(t, root, "com.example.app", ver, uint64(100+i), key)
		if _, err := mod.Install(context.Background(), InstallTransaction{
			ManifestBytes: mb, SigBlock: sig, StagingDir: staging, Source: SourceDynamicInstall,
		}); err != nil {
			t.Fatalf("Install %s: %v", ver, err)
		}
	}
	for i, req := range auth.installed {
		if req.ReplaceExisting {
			t.Errorf("unexpected package registry result; value = %d ReplaceExisting", i+1)
		}
	}
}

//

func TestInstall_FailedReplaceKeepsCodeDir(t *testing.T) {
	mod, auth, _, _ := newTestInstaller(t)
	root := t.TempDir()
	key := newDevKey(t)

	staging, mb, sig := newValidStagingWithKey(t, root, "com.example.app", "1.0.0", 100, key)
	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: mb, SigBlock: sig, StagingDir: staging, Source: SourceDynamicInstall,
	}); err != nil {
		t.Fatalf("unexpected package registry result; value = %v", err)
	}
	auth.removed = nil

	//

	auth.appUserErr = errors.New("injected failure after code landed")
	staging2, mb2, sig2 := newValidStagingWithKey(t, root, "com.example.app", "1.0.0", 100, key)
	_, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: mb2, SigBlock: sig2, StagingDir: staging2, Source: SourceDynamicInstall,
	})
	if err == nil {
		t.Fatal("unexpected package registry result; Install")
	}

	for _, r := range auth.removed {
		if r.Root == mod.packageRoot {
			t.Fatalf("unexpected package registry result; value = %s", r.Path)
		}
	}
}

func TestProvision_CreatesUserAndDataDir(t *testing.T) {

	auth := &fakeInstaller{}
	m := newProvisionModule(t, auth)

	e := Entry{
		Manifest: Manifest{PackageID: "nervus.example"},
		UID:      20005,
		Source:   SourceSystemImage,
	}
	if err := m.provisionEntry(context.Background(), e); err != nil {
		t.Fatalf("provisionEntry: %v", err)
	}

	if len(auth.appUsers) != 1 {
		t.Fatalf("unexpected package registry result; EnsureAppUser %d, want 1", len(auth.appUsers))
	}
	u := auth.appUsers[0]
	if u.UID != 20005 || u.GID != 20005 {
		t.Errorf("unexpected package registry result; uid/gid = %d/%d, want 20005/20005 GID UID", u.UID, u.GID)
	}
	if u.Name != authority.AppUserName(20005) {
		t.Errorf("name = %q, want %q", u.Name, authority.AppUserName(20005))
	}

	if len(auth.dataDirs) != 1 {
		t.Fatalf("unexpected package registry result; CreatePrivateDataDirectory %d, want 1", len(auth.dataDirs))
	}
	d := auth.dataDirs[0]
	if d.Perm != 0o700 {
		t.Errorf("unexpected package registry result; perm = %#o, want 0700", d.Perm)
	}
	if d.UID != 20005 {
		t.Errorf("data dir uid = %d, want 20005", d.UID)
	}
}

func TestProvision_IsIdempotent(t *testing.T) {

	auth := &fakeInstaller{dataDirErr: fmt.Errorf("%w: nervus.example", authority.ErrAlreadyExists)}
	m := newProvisionModule(t, auth)

	e := Entry{Manifest: Manifest{PackageID: "nervus.example"}, UID: 20006}
	if err := m.provisionEntry(context.Background(), e); err != nil {
		t.Fatalf("unexpected package registry result; value = %v", err)
	}
}

func TestProvision_RealDataDirErrorStillFails(t *testing.T) {

	auth := &fakeInstaller{dataDirErr: errors.New("permission denied")}
	m := newProvisionModule(t, auth)

	e := Entry{Manifest: Manifest{PackageID: "nervus.example"}, UID: 20007}
	if err := m.provisionEntry(context.Background(), e); err == nil {
		t.Fatal("unexpected package registry result")
	}
}

func TestProvisionAll_OneFailureDoesNotBlockOthers(t *testing.T) {

	auth := &fakeInstaller{appUserErr: errors.New("boom")}
	m := newProvisionModule(t, auth)

	entries := []Entry{
		{Manifest: Manifest{PackageID: "a"}, UID: 20010},
		{Manifest: Manifest{PackageID: "b"}, UID: 20011},
	}
	ok := m.provisionAll(context.Background(), entries)
	if ok != 0 {
		t.Errorf("unexpected package registry result; ok = %d, want 0", ok)
	}

	if len(auth.appUsers) != 2 {
		t.Fatalf("unexpected package registry result; value = %d", len(auth.appUsers))
	}
}

// TestInstall_RejectsContentChangedAfterConfirmation pins the confirmation screen binding.
//
// The .nspkg sits in the user-data area shared across packages, so the caller can swap the file between Inspect and Install.
// Each call is legitimate on its own, and neither reveals anything when looked at alone; but the user agreed based on A's permission
// list, while ConsentedPermissions would be applied to B.
func TestInstall_RejectsContentChangedAfterConfirmation(t *testing.T) {
	mod, auth, idReg, _ := newTestInstaller(t)
	root := t.TempDir()
	staging, manifestBytes, sig := newValidStaging(t, root, "com.example.app", "1.0.0")

	_, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes, SigBlock: sig, StagingDir: staging,
		Source: SourceDynamicInstall,
		// What the user saw when confirming was different content
		ExpectedManifestDigest: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if !errors.Is(err, ErrManifestDigestMismatch) {
		t.Fatalf("err = %v, want ErrManifestDigestMismatch", err)
	}
	// It must be refused before any state is touched
	if len(auth.installed) != 0 || len(auth.dataDirs) != 0 {
		t.Fatalf("Authority shouldn't be called: installed=%d dataDirs=%d", len(auth.installed), len(auth.dataDirs))
	}
	if len(idReg.replaced) != 0 {
		t.Fatal("identity shouldn't be updated")
	}
	if mod.registry.Len() != 0 {
		t.Fatal("the Registry shouldn't be updated")
	}
}

// TestInstall_AcceptsMatchingDigest: when the digest Inspect returned comes back unchanged, the install goes ahead.
//
// Together with the previous test, this shows the gate judges "by content" rather than "refuse whenever a digest is present".
func TestInstall_AcceptsMatchingDigest(t *testing.T) {
	mod, _, _, _ := newTestInstaller(t)
	root := t.TempDir()
	staging, manifestBytes, sig := newValidStaging(t, root, "com.example.app", "1.0.0")

	digest := hex.EncodeToString(sha256Sum(manifestBytes))
	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes, SigBlock: sig, StagingDir: staging,
		Source:                 SourceDynamicInstall,
		ExpectedManifestDigest: digest,
	}); err != nil {
		t.Fatalf("refused even though the digest matches: %v", err)
	}
	if mod.registry.Len() != 1 {
		t.Fatalf("Registry entry count = %d, want 1", mod.registry.Len())
	}
}

// TestInstall_EmptyDigestSkipsBinding: empty = no check.
//
// These semantics must be kept: system setup scripts and deploy call Install directly and never have an Inspect
// step. If empty were checked too, they'd all break, and since that's the setup flow, the symptom would be a freshly flashed machine
// with not one package installed.
func TestInstall_EmptyDigestSkipsBinding(t *testing.T) {
	mod, _, _, _ := newTestInstaller(t)
	root := t.TempDir()
	staging, manifestBytes, sig := newValidStaging(t, root, "com.example.app", "1.0.0")

	if _, err := mod.Install(context.Background(), InstallTransaction{
		ManifestBytes: manifestBytes, SigBlock: sig, StagingDir: staging,
		Source: SourceDynamicInstall,
		// ExpectedManifestDigest left empty
	}); err != nil {
		t.Fatalf("an empty digest should skip the check, but it failed: %v", err)
	}
}

// TestInspect_DigestMatchesInstallExpectation pins that both sides compute the same value.
//
// Inspect and Install each compute the digest once. If the two used different inputs (say one over the raw bytes,
// the other over a re-serialised struct), every install carrying a digest would fail, and that's a fault that only shows up
// when "the confirmation screen goes through the full flow"; unit tests on either side wouldn't see it.
func TestInspect_DigestMatchesInstallExpectation(t *testing.T) {
	mod, _, _, _ := newTestInstaller(t)
	root := t.TempDir()
	staging, manifestBytes, _ := newValidStaging(t, root, "com.example.app", "1.0.0")

	got, err := mod.Inspect(context.Background(), staging)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	want := hex.EncodeToString(sha256Sum(manifestBytes))
	if got.ManifestDigest != want {
		t.Fatalf("Inspect's digest = %q, Install expects %q; the two algorithms disagree, "+
			"and every install carrying a digest would fail", got.ManifestDigest, want)
	}
}
