package main

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zeroReader is an endless source of zero bytes, used to fill up the size a header declares.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// entry is one tar entry for building a test .nspkg.
type entry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	linkname string
	// A non-zero sizeLie is written as the header's Size instead of len(body).
	// Used to check CopyN's safety net: the header can lie.
	sizeLie int64
}

// buildNspkg packs entries into a zstd+tar .nspkg and returns its path.
func buildNspkg(t *testing.T, entries []entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pkg.nspkg")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()

	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if tf != tar.TypeReg {
			size = 0
		}
		if e.sizeLie != 0 {
			size = e.sizeLie
		}
		hdr := &tar.Header{Name: e.name, Typeflag: tf, Mode: mode, Size: size, Linkname: e.linkname}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		switch {
		case tf != tar.TypeReg:
			// Non-regular types have no body.
		case e.sizeLie != 0:
			// tar.Writer requires writing every byte the header declares, or Close reports
			// "missed writing N bytes". Zeros are fine: zstd compresses them to almost nothing,
			// and Unpack refuses on hdr.Size before reading the body, so it never gets here.
			if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
				t.Fatalf("pad body %q: %v", e.name, err)
			}
		default:
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zstd: %v", err)
	}
	return p
}

// unpackInto unpacks entries into a new staging directory and returns that directory and the error.
func unpackInto(t *testing.T, entries []entry) (string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	return dest, Unpack(buildNspkg(t, entries), dest)
}

// A normal package must unpack, including nested directories.
//
// This comes first: everything after it is a refusal case, and without one success case
// an implementation that "refuses everything" would turn the whole file green.
func TestUnpack_NormalPackage(t *testing.T) {
	dest, err := unpackInto(t, []entry{
		{name: "manifest.json", body: `{"schema":1}`},
		{name: "manifest.sig", body: `{"format":1}`},
		{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/app", body: "ELF", mode: 0o755},
		{name: "lib/native/libfoo.so", body: "SO", mode: 0o644},
	})
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	for _, rel := range []string{"manifest.json", "manifest.sig", "bin/app", "lib/native/libfoo.so"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	// The executable bit must be kept. Lose it and the installed package can't run, with 203/EXEC as the symptom,
	// and nobody thinks to blame unpacking for dropping a permission bit.
	fi, err := os.Stat(filepath.Join(dest, "bin/app"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/app mode = %v, want executable", fi.Mode().Perm())
	}
}

// ".." escape: the classic tar-slip.
func TestUnpack_RejectsDotDotEscape(t *testing.T) {
	for _, name := range []string{
		"../evil",
		"../../etc/cron.d/evil",
		"bin/../../../evil",
		"./../evil",
	} {
		t.Run(name, func(t *testing.T) {
			dest, err := unpackInto(t, []entry{{name: name, body: "pwned"}})
			if err == nil {
				t.Fatalf("Unpack accepted escaping entry %q", name)
			}
			// It's not enough to report an error; it must also [not have written anything]. An error after writing is no protection.
			outside := filepath.Join(filepath.Dir(dest), "evil")
			if _, statErr := os.Stat(outside); statErr == nil {
				t.Errorf("entry %q was written outside dest at %s", name, outside)
			}
		})
	}
}

// Absolute-path entries.
func TestUnpack_RejectsAbsolutePath(t *testing.T) {
	_, err := unpackInto(t, []entry{{name: "/etc/passwd", body: "pwned"}})
	if err == nil {
		t.Fatal("Unpack accepted an absolute entry name")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("err = %v, want it to mention 'absolute'", err)
	}
}

// Empty entry name.
func TestUnpack_RejectsEmptyName(t *testing.T) {
	_, err := unpackInto(t, []entry{{name: "", body: "x"}})
	if err == nil {
		t.Fatal("Unpack accepted an empty entry name")
	}
}

// Symlinks and hard links: a symlink pointing outside the target directory is an escape route,
// and neither has a legitimate use in a signed App package.
func TestUnpack_RejectsLinks(t *testing.T) {
	cases := []struct {
		label string
		e     entry
	}{
		{"symlink", entry{name: "evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
		{"hardlink", entry{name: "evil", typeflag: tar.TypeLink, linkname: "manifest.json"}},
		{"symlink-relative", entry{name: "bin/x", typeflag: tar.TypeSymlink, linkname: "../../../etc"}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			if _, err := unpackInto(t, []entry{tc.e}); err == nil {
				t.Fatalf("Unpack accepted a %s entry", tc.label)
			}
		})
	}
}

// Device nodes and FIFOs.
func TestUnpack_RejectsSpecialFiles(t *testing.T) {
	cases := []struct {
		label string
		tf    byte
	}{
		{"chardev", tar.TypeChar},
		{"blockdev", tar.TypeBlock},
		{"fifo", tar.TypeFifo},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			if _, err := unpackInto(t, []entry{{name: "evil", typeflag: tc.tf}}); err == nil {
				t.Fatalf("Unpack accepted a %s entry", tc.label)
			}
		})
	}
}

// setuid / setgid / sticky bits must be stripped.
//
// tar's Mode is the full low bits of st_mode and can carry 04000. A setuid-root file
// landing on disk is privilege escalation, even if nervud then passes the content against the digest, because permission bits
// aren't covered by the digest.
func TestUnpack_StripsSetuidBits(t *testing.T) {
	dest, err := unpackInto(t, []entry{
		{name: "bin/suid", body: "x", mode: 0o4755},
		{name: "bin/sgid", body: "x", mode: 0o2755},
		{name: "bin/sticky", body: "x", mode: 0o1755},
	})
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	for _, rel := range []string{"bin/suid", "bin/sgid", "bin/sticky"} {
		fi, err := os.Stat(filepath.Join(dest, rel))
		if err != nil {
			t.Fatal(err)
		}
		if m := fi.Mode(); m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			t.Errorf("%s mode = %v, want setuid/setgid/sticky stripped", rel, m)
		}
	}
}

// A duplicate entry name must fail, never overwrite the earlier one.
//
// tar allows duplicate entry names. With overwriting, an attacker could let the first entry pass what nervud's
// digest check sees, then replace it with the second; O_EXCL closes that route.
func TestUnpack_RejectsDuplicateEntries(t *testing.T) {
	_, err := unpackInto(t, []entry{
		{name: "bin/app", body: "good"},
		{name: "bin/app", body: "evil"},
	})
	if err == nil {
		t.Fatal("Unpack accepted a duplicate entry name; later content would silently win")
	}
}

// Oversized entry: refuse when the Size the header claims exceeds the limit.
func TestUnpack_RejectsOversizedEntry(t *testing.T) {
	_, err := unpackInto(t, []entry{{name: "big", sizeLie: maxEntryBytes + 1}})
	if err == nil {
		t.Fatal("Unpack accepted an entry over the size cap")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v, want it to mention 'too large'", err)
	}
}

// Entry count limit: bombs made of huge numbers of small entries.
func TestUnpack_RejectsTooManyEntries(t *testing.T) {
	entries := make([]entry, 0, maxTotalEntries+2)
	for i := 0; i <= maxTotalEntries+1; i++ {
		entries = append(entries, entry{name: "f" + itoa(i), body: "x"})
	}
	_, err := unpackInto(t, entries)
	if err == nil {
		t.Fatal("Unpack accepted an archive over the entry-count cap")
	}
	if !strings.Contains(err.Error(), "too many entries") {
		t.Errorf("err = %v, want it to mention 'too many entries'", err)
	}
}

// safeJoin and a sibling directory with the same prefix: /a/staging-evil must not pass a check for /a/staging.
//
// Tested on safeJoin directly rather than through Unpack because building that scenario needs a real
// same-prefix sibling directory, and the property itself is only about string comparison.
func TestSafeJoin_SiblingPrefixIsNotInside(t *testing.T) {
	base := filepath.Clean("/var/lib/nervus/staging")
	if _, err := safeJoin(base, "../staging-evil/x"); err == nil {
		t.Fatal("safeJoin accepted a sibling directory sharing the base prefix")
	}
}

// safeJoin accepts normal paths.
func TestSafeJoin_AcceptsNormalNames(t *testing.T) {
	base := filepath.Clean("/var/lib/nervus/staging")
	for _, name := range []string{"manifest.json", "bin/app", "lib/native/libfoo.so", "./bin/app"} {
		got, err := safeJoin(base, name)
		if err != nil {
			t.Errorf("safeJoin(%q) = %v, want ok", name, err)
			continue
		}
		if !strings.HasPrefix(got, base+string(os.PathSeparator)) {
			t.Errorf("safeJoin(%q) = %q, want inside %q", name, got, base)
		}
	}
}

// itoa avoids bringing in anything beyond strconv for a one-line number conversion.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
