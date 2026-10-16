package catalog

import "fmt"

// SourceError identifies which source made this Build fail.
//
// # Why it's needed
//
// Build is all or nothing: if any one source is invalid, the whole Catalog can't be built.
// For the install path that's exactly the semantics we want: if the new package has a problem, refuse it and leave the existing system alone.
//
// But the boot scan can't have those semantics. The comment on pkgregistry.Module's Start promises
// "one bad package must not bring down the whole kernel start sequence", but Scan's per-package skipping only covers manifest parsing,
// digests and signature checks. Catalog-level conflicts (a resource with several managers, inconsistent interface contracts,
// namespace overreach) used to make Start return an error outright, and the whole machine couldn't boot.
//
// To quarantine the culprit at boot, the caller has to know who the culprit is. Take the PackageID out with errors.As
// rather than parsing the error string: error text changes, and this decision determines whether a machine can boot.
type SourceError struct {
	PackageID string
	Err       error
}

func (e *SourceError) Error() string {
	if e == nil {
		return "catalog: source error"
	}
	return fmt.Sprintf("catalog: source %q: %v", e.PackageID, e.Err)
}

func (e *SourceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// sourceErrorf wraps a failure tied to one source in a SourceError.
func sourceErrorf(packageID string, format string, args ...any) error {
	return &SourceError{PackageID: packageID, Err: fmt.Errorf(format, args...)}
}
