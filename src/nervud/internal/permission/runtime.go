// This file is runtime permission grant/revoke: the (Package, permission) -> GrantState state machine for GrantUser (dangerous)
// permissions, persisted to the registry directory, with revocation of
// motion-group permissions linked to control revoking leases + bumping the motion epoch.
//
// This is an immediate-revocation ability Android doesn't have: after a revoke, copy-on-write + an atomic pointer
// let the next Allowed see it straight away, with no process restart needed.
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/nervus-os/nervud/internal/audit"
)

const grantStateFile = "_grants.json"

// LeaseRevoker is the narrow dependency on control used when revoking motion-group permissions: it revokes every
// ControlLease a Package holds, and control bumps the motion epoch. permission
// doesn't touch the gate directly: epoch semantics belong to motion revocation, not to permissions. Skipped when nil (not yet wired).
type LeaseRevoker interface {
	RevokeByPackage(pkgID string) error
}

// grantKey is the key of runtime grant state
type grantKey struct {
	pkg  string
	perm string
}

// grantStore is the persistence container for runtime grant state.
//
// It's separate from the Registry's install set (copy-on-write atomic pointer): the install set is driven by
// pkgregistry's full projection, while runtime state is driven by user confirmation/revocation, so they have different update sources. grantStore uses
// an ordinary lock + a disk write on every change; grant/revoke isn't a hot path and needs no lock-free reads.
type grantStore struct {
	mu       sync.RWMutex
	stateDir string       // directory on disk (/var/lib/nervus/registry); empty means not persisted (tests / not wired)
	revoker  LeaseRevoker // hook for revoking motion-group permissions; may be nil
	aud      audit.Recorder
	states   map[grantKey]GrantState
}

func newGrantStore() *grantStore {
	return &grantStore{states: make(map[grantKey]GrantState)}
}

// diskGrant is one record of the on-disk JSON
type diskGrant struct {
	PackageID  string     `json:"package_id"`
	Permission string     `json:"permission"`
	State      GrantState `json:"state"`
}

// load reads runtime grant state back from stateDir; a missing or corrupt file counts as empty (conservative: a grant
// that can't be read must never be treated as granted).
func (g *grantStore) load() {
	if g.stateDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(g.stateDir, grantStateFile))
	if err != nil {
		return
	}
	var recs []diskGrant
	if err := json.Unmarshal(data, &recs); err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range recs {
		if r.PackageID == "" || r.Permission == "" || !r.State.valid() {
			continue
		}
		g.states[grantKey{r.PackageID, r.Permission}] = r.State
	}
}

// state returns the runtime state of (pkg, perm); no record means NotRequested
func (g *grantStore) state(pkg, perm string) GrantState {
	g.mu.RLock()
	defer g.mu.RUnlock()
	s, ok := g.states[grantKey{pkg, perm}]
	if !ok {
		return GrantStateNotRequested
	}
	return s
}

// persistLocked writes all current state to disk atomically (caller holds the write lock)
func (g *grantStore) persistLocked() error {
	if g.stateDir == "" {
		return nil
	}
	recs := make([]diskGrant, 0, len(g.states))
	for k, s := range g.states {
		recs = append(recs, diskGrant{PackageID: k.pkg, Permission: k.perm, State: s})
	}
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("permission: encode grants: %w", err)
	}
	if err := os.MkdirAll(g.stateDir, 0o700); err != nil {
		return fmt.Errorf("permission: create grant state dir: %w", err)
	}
	return writeFileAtomic(filepath.Join(g.stateDir, grantStateFile), data, 0o600)
}

// set updates the runtime state of (pkg, perm) and writes it to disk. If this turns a motion-group permission into non-granted
//
// (revoked/denied), the revoker revokes leases + bumps the motion epoch.
//
// The caller (the Registry, which holds the Catalog) decides isMotionGroup and passes it in: grantStore doesn't hold
// the Catalog, so it doesn't manage both state and definitions.
func (g *grantStore) set(pkg, perm string, state GrantState, isMotionGroup bool) error {
	g.mu.Lock()
	key := grantKey{pkg, perm}
	prev, had := g.states[key]
	g.states[key] = state
	if err := g.persistLocked(); err != nil {
		// If the disk write fails, roll back memory; memory must never run ahead of disk (after a restart the old disk value would win, causing inconsistency)
		if had {
			g.states[key] = prev
		} else {
			delete(g.states, key)
		}
		g.mu.Unlock()
		return err
	}
	g.mu.Unlock()

	// Going from granted to non-granted = revocation. Revoking a motion-group permission must make control revoke all of this package's
	// leases (including bumping the epoch), or an App that already holds a lease could keep moving the robot.
	revoked := prev == GrantStateGranted && state != GrantStateGranted
	if revoked && isMotionGroup && g.revoker != nil {
		if rerr := g.revoker.RevokeByPackage(pkg); rerr != nil && g.aud != nil {
			g.aud.Record(context.Background(), audit.Event{
				Action: "permission.revoke.motion", Subject: pkg, Denied: true, Err: rerr,
			})
		}
	}
	if g.aud != nil {
		g.aud.Record(context.Background(), audit.Event{
			Action:  "permission.SetRuntimeState",
			Subject: pkg,
			Detail:  fmt.Sprintf("%s state=%d (was %d)", perm, state, prev),
		})
	}
	return nil
}

// clearPackage deletes all runtime grant state of a Package and writes it to disk. Called on uninstall; otherwise reinstalling
// the same ID would inherit old dangerous permission grants (fix).
func (g *grantStore) clearPackage(pkg string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	removed := make(map[grantKey]GrantState)
	for k, state := range g.states {
		if k.pkg == pkg {
			removed[k] = state
			delete(g.states, k)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := g.persistLocked(); err != nil {
		for key, state := range removed {
			g.states[key] = state
		}
		return err
	}
	return nil
}

// writeFileAtomic writes a file atomically (temporary file, then rename). permission's runtime grant state
// is bookkeeping in nervud's own registry directory and doesn't cross a trust boundary, so it uses the standard library's os (same
// reason as pkgregistry.writeFileAtomic).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-grants-*")
	if err != nil {
		return fmt.Errorf("permission: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("permission: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("permission: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("permission: close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("permission: chmod temp: %w", err)
	}
	return os.Rename(tmpPath, path)
}
