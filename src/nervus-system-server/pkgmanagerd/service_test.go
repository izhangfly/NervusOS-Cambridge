package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	pkgv1 "github.com/nervus-os/nervus-ipc/protocol/interface/pkgmanagerv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

func TestResolveHandoff_RejectsEscapes(t *testing.T) {
	// The hand-off directory is the only writable path in this service's sandbox. If a caller's relative path could escape it,
	// the App would be choosing an arbitrary file for this process to read, and what this process's UID can read
	// isn't necessarily what the caller can.
	bad := []struct {
		name string
		path string
	}{
		{"empty path", ""},
		{"absolute path", "/etc/shadow"},
		{"one level up", "../other-package/x.nspkg"},
		{"several levels up", "../../../../etc/shadow"},
		{"up from the middle", "sub/../../escape.nspkg"},
		{"only up", ".."},
		{"current directory", "."},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveHandoff(tc.path)
			if err == nil {
				t.Fatalf("must be refused, but resolved to %q", got)
			}
			if !errors.Is(err, ErrUnsafeRelPath) {
				t.Errorf("err = %v, want ErrUnsafeRelPath", err)
			}
		})
	}
}

func TestResolveHandoff_SiblingPrefixIsNotInside(t *testing.T) {
	// Without the separator in the prefix comparison, a sibling directory called <handoffRoot>-evil would pass the check
	// (/var/lib/nervus/user-data-evil starts with /var/lib/nervus/user-data).
	// This assertion pins that separator.
	//
	// The path follows handoffRoot: hard-code some other name and, once the hand-off directory changes, this test
	// would no longer test what it claims to.
	if _, err := resolveHandoff("../" + filepath.Base(handoffRoot) + "-evil/x.nspkg"); err == nil {
		t.Fatal("a sibling directory (same prefix) must be refused")
	}
}

func TestResolveHandoff_AcceptsNormalPaths(t *testing.T) {
	ok := []string{"app.nspkg", "downloads/app.nspkg", "a/b/c.nspkg", "./app.nspkg"}
	for _, p := range ok {
		got, err := resolveHandoff(p)
		if err != nil {
			t.Errorf("%q should be accepted: %v", p, err)
			continue
		}
		if !strings.HasPrefix(got, handoffRoot+"/") {
			t.Errorf("%q → %q, not inside the hand-off directory", p, got)
		}
	}
}

func TestClassify_MapsKernelMessages(t *testing.T) {
	// These are keywords that appear in nervud's real error text. Misclassifying them makes the UI give
	// misleading advice (for example calling "package corrupt" "version too old").
	tests := []struct {
		msg  string
		want pkgv1.PackageManagerReason
	}{
		{"pkgregistry: signature verification failed", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_SIGNATURE_INVALID},
		{"no developer signature", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_SIGNATURE_INVALID},
		{"pkgregistry: digest mismatch: {Missing:[bin/x]}", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DIGEST_MISMATCH},
		{"upgrade rejected: version_code 1 <= 2", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DOWNGRADE},
		{"manifest has empty or invalid supported_abis", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ABI_MISMATCH},
		{"pkgregistry: lineage broken", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_LINEAGE_BROKEN},
		{"system-image package cannot be uninstalled (immutable)", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_IMMUTABLE},
		{"unpack failed: archive entry escapes destination", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ARCHIVE_INVALID},
		{"uninstall refused (not-found): no such package", pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_PACKAGE_NOT_FOUND},

		// Content swapped after confirmation. [Both of these texts contain "digest"], and classify
		// matches substrings in order, so if this case came after the "digest" one it would be reported as
		// "package corrupt, download it again", when the right fix is to go through confirmation again.
		//
		// Not hypothetical: nervud's actual text is
		// "pkgregistry: manifest digest does not match the confirmed package".
		{
			"pkgregistry: manifest digest does not match the confirmed package",
			pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_CONTENT_CHANGED,
		},
		{
			"install refused: package content changed after user confirmation",
			pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_CONTENT_CHANGED,
		},
	}
	for _, tc := range tests {
		if got := classify(tc.msg); got != tc.want {
			t.Errorf("classify(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestClassify_UnknownFailsClosed(t *testing.T) {
	// If it can't be recognised, say so. Guessing a possibly misleading cause is worse than admitting not knowing:
	// users would waste effort following the wrong advice.
	got := classify("something entirely unexpected happened")
	if got != pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_UNSPECIFIED {
		t.Fatalf("unknown error = %v, want UNSPECIFIED", got)
	}
}

func TestCodeFor_MatchesReasonSemantics(t *testing.T) {
	// The outer code decides the SDK's recovery behaviour (IsRetryable / NeedsReResolve),
	// the typed detail decides how the UI explains it. They must agree, or the SDK and UI would contradict each other.
	tests := []struct {
		reason pkgv1.PackageManagerReason
		want   ipcv1.StatusCode
	}{
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_PACKAGE_NOT_FOUND, ipcv1.StatusCode_STATUS_CODE_NOT_FOUND},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_MANIFEST_INVALID, ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_ARCHIVE_INVALID, ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_SIGNATURE_INVALID, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_DOWNGRADE, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_IMMUTABLE, ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION},
		{pkgv1.PackageManagerReason_PACKAGE_MANAGER_REASON_UNSPECIFIED, ipcv1.StatusCode_STATUS_CODE_INTERNAL},
	}
	for _, tc := range tests {
		if got := codeFor(tc.reason); got != tc.want {
			t.Errorf("codeFor(%v) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

func TestMethodIDs_MatchProtoEnum(t *testing.T) {
	// If the local constants drift from the proto enum, calls are routed to the wrong method, discovered only at run time,
	// where it looks like "the feature is wrong". This assertion surfaces the drift at compile/test time.
	if methodInstall != 1 || methodUninstal != 2 || methodList != 3 || methodSetComp != 4 {
		t.Fatalf("method id doesn't match the proto: install=%d uninstall=%d list=%d setcomp=%d",
			methodInstall, methodUninstal, methodList, methodSetComp)
	}
}
