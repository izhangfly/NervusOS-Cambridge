// staging.go: if the CLI crashes or gives up after begin-staging and before install, it leaves an uncommitted directory in the staging root. These don't affect correctness
//
// (install only accepts explicitly submitted paths), but they take up disk, so each begin-staging reclaims old ones
//
// on a best-effort basis.
package admin

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleStagingAge is the age after which a staging directory counts as an orphan that can be reclaimed. 1 hour:
// far longer than any normal begin -> unpack -> install (seconds), without letting orphans pile up for long.
// The sweep is best-effort: it would rather miss one than delete one in use.
const staleStagingAge = time.Hour

// stagingPrefix is the prefix begin-staging uses for directories (os.MkdirTemp's pattern). The sweep
// only touches directories with this prefix, never anything else under the staging root.
const stagingPrefix = "stage-"

// sweepStaleStaging deletes stage-* directories under the staging root older than staleStagingAge. Entirely
// best-effort: any failing step is only logged and doesn't block begin-staging (reclaiming disk shouldn't break installs).
func (s *Server) sweepStaleStaging() {
	entries, err := os.ReadDir(s.stagingRoot)
	if err != nil {
		// The staging root doesn't exist yet / can't be read: begin-staging's MkdirTemp will give a clearer error,
		// so just return quietly here.
		return
	}
	cutoff := time.Now().Add(-staleStagingAge)
	for _, ent := range entries {
		if !ent.IsDir() || !strings.HasPrefix(ent.Name(), stagingPrefix) {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // still young; a CLI may be using it
		}
		dir := filepath.Join(s.stagingRoot, ent.Name())
		if err := os.RemoveAll(dir); err != nil {
			s.log.Warn("admin: failed to sweep stale staging dir", "dir", dir, "err", err)
		}
	}
}
