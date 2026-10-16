package permission

import "testing"

const storageUser = "perm.storage.user"

// USER_CONSENT permissions of system software need no runtime consent.
//
// Before this, a file manager shipped with the image could never get perm.storage.user once installed:
// the runtime state defaults to NOT_REQUESTED, and the only thing in the whole system that can change it is nervusctl on the admin channel.
// The symptom was the UI reporting "user directory not writable", with nowhere to fix it.
func TestAllowed_SystemSoftwareSkipsRuntimeConsent(t *testing.T) {
	r := NewDefaultRegistry()
	if err := r.Replace([]Grant{{
		PackageID:     "nervus.filemanager",
		Permissions:   []string{storageUser},
		ConsentExempt: true,
	}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if got := r.GrantStateOf("nervus.filemanager", storageUser); got != GrantStateNotRequested {
		t.Fatalf("runtime state = %v, want NOT_REQUESTED (the exemption must not fake a grant record)", got)
	}
	if !r.Allowed("nervus.filemanager", storageUser) {
		t.Fatal("system software still required runtime consent")
	}
}

// Ordinary apps still need runtime consent: permissions like the camera, user files and motion control
// must be asked for each time.
func TestAllowed_OrdinaryAppStillNeedsRuntimeConsent(t *testing.T) {
	r := NewDefaultRegistry()
	if err := r.Replace([]Grant{{
		PackageID:   "com.example.app",
		Permissions: []string{storageUser},
		// ConsentExempt is false: not system software shipped with the image
	}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if r.Allowed("com.example.app", storageUser) {
		t.Fatal("an ordinary app got a USER_CONSENT permission without consent")
	}
	if err := r.SetRuntimeState("com.example.app", storageUser, GrantStateGranted); err != nil {
		t.Fatalf("SetRuntimeState: %v", err)
	}
	if !r.Allowed("com.example.app", storageUser) {
		t.Fatal("still not allowed after consent")
	}
}

// The exemption must NOT bypass the install-time decision: system software still only gets what IntersectAt approved.
//
// Mixing the two up would make "shipped with the image" a master pass: writing one more
// permission into the manifest would be the same as getting it.
func TestAllowed_ExemptionDoesNotBypassInstallSet(t *testing.T) {
	r := NewDefaultRegistry()
	if err := r.Replace([]Grant{{
		PackageID: "nervus.filemanager",
		// Nothing was granted at install time
		Permissions:   nil,
		ConsentExempt: true,
	}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if r.Allowed("nervus.filemanager", storageUser) {
		t.Fatal("the exemption bypassed the install-time set")
	}
}

// The exemption is swapped in atomically with Replace. After a package stops being system software and becomes an ordinary one (in theory only
// when it's reinstalled as a dynamic package), the next Allowed must see the new decision immediately.
func TestAllowed_ExemptionIsReplacedAtomically(t *testing.T) {
	r := NewDefaultRegistry()
	if err := r.Replace([]Grant{{
		PackageID: "com.example.app", Permissions: []string{storageUser}, ConsentExempt: true,
	}}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if !r.Allowed("com.example.app", storageUser) {
		t.Fatal("the exemption didn't take effect")
	}

	if err := r.Replace([]Grant{{
		PackageID: "com.example.app", Permissions: []string{storageUser},
	}}); err != nil {
		t.Fatalf("Replace again: %v", err)
	}
	if r.Allowed("com.example.app", storageUser) {
		t.Fatal("still allowed after the exemption was removed")
	}
}
