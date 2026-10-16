// This file holds the developer options that affect install decisions.
//
// Why the authoritative state is in the kernel rather than settingsd: these switches relax install decisions directly, so the kernel must
// hold and read them itself; they can't be a file that ordinary configuration write permissions could change. It lives under
// /var/lib/nervus/registry/ (0700, readable and writable only by nervud), in the same domain as the other bookkeeping state.
//
// Two things are never relaxed (enforced in install.go/upgrade.go, not expressed here):
//   - the trust ceiling: even with allow_unverified_signature, the package is still Ordinary;
//   - signing continuity (lineage succession): protection against identity hijack and data theft isn't relaxed by devmode either.
package pkgregistry

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const devModeStateFile = "_devmode.json"

// DevMode is the set of developer switches relevant to install decisions.
//
// Only fields install decisions use are here. debug_bridge / stay_awake and so on belong to service/session
// and aren't duplicated here
type DevMode struct {
	Enabled                  bool `json:"-"`
	AllowUnverifiedSignature bool `json:"allow_unverified_signature"`
	AllowDowngrade           bool `json:"allow_downgrade"`
	SkipOEMCountersign       bool `json:"skip_oem_countersign"`
}

// On disk: { "enabled": bool, "options": {... } }
type devModeFile struct {
	Enabled bool    `json:"enabled"`
	Options DevMode `json:"options"`
}

// loadDevMode reads the devmode bookkeeping file; a missing or corrupt file returns everything off.
//
// It's read fresh on every install rather than cached: devmode can be switched at run time from system settings, and caching would let a switch just turned off
// still take effect at the next install. When corrupt it's conservatively treated as all off: a devmode that can't be read
// must never be treated as relaxing everything
func loadDevMode(stateDir string) DevMode {
	data, err := os.ReadFile(filepath.Join(stateDir, devModeStateFile))
	if err != nil {
		return DevMode{}
	}
	var f devModeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return DevMode{}
	}
	opts := f.Options
	opts.Enabled = f.Enabled
	if !f.Enabled {
		// When developer mode isn't enabled, no individual switch takes effect
		return DevMode{}
	}
	return opts
}
