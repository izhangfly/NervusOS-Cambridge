// Package wslg reports launcher-owned WSLg prerequisites without modifying
// mounts, processes, Weston state, or the existing keepalive workaround.
package wslg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

const defaultCopyModeWindow = 2 * time.Minute

type Config struct {
	RequireDisplay       bool
	RequireKeepalive     bool
	RequireSharedMemory  bool
	RequireCopyModeCheck bool
	ProcessRoot          string
	WSLgRoot             string
	SharedMemoryPath     string
	WestonLogPath        string
	KeepaliveTitle       string
	CopyModeWindow       time.Duration
	Environment          map[string]string
	Now                  func() time.Time
}

type Probe struct{ config Config }

func DefaultConfig() Config {
	return Config{
		RequireDisplay:       true,
		RequireKeepalive:     true,
		RequireSharedMemory:  true,
		RequireCopyModeCheck: true,
		ProcessRoot:          "/proc",
		WSLgRoot:             "/mnt/wslg",
		SharedMemoryPath:     "/mnt/shared_memory",
		WestonLogPath:        "/mnt/wslg/weston.log",
		KeepaliveTitle:       "NervusWSLgKeepalive",
		CopyModeWindow:       defaultCopyModeWindow,
		Now:                  time.Now,
	}
}

func New(config Config) (*Probe, error) {
	defaults := DefaultConfig()
	if config.ProcessRoot == "" {
		config.ProcessRoot = defaults.ProcessRoot
	}
	if config.WSLgRoot == "" {
		config.WSLgRoot = defaults.WSLgRoot
	}
	if config.SharedMemoryPath == "" {
		config.SharedMemoryPath = defaults.SharedMemoryPath
	}
	if config.WestonLogPath == "" {
		config.WestonLogPath = defaults.WestonLogPath
	}
	if config.KeepaliveTitle == "" {
		config.KeepaliveTitle = defaults.KeepaliveTitle
	}
	if config.CopyModeWindow == 0 {
		config.CopyModeWindow = defaults.CopyModeWindow
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.CopyModeWindow < 0 {
		return nil, errors.New("wslg: copy-mode window must not be negative")
	}
	return &Probe{config: config}, nil
}

func (p *Probe) Check(ctx context.Context) []simulation.WSLgFaultEvent {
	if p == nil || ctx.Err() != nil {
		return nil
	}
	faults := make([]simulation.WSLgFaultEvent, 0, 4)
	appendFault := func(kind simulation.WSLgFaultKind) {
		faults = append(faults, simulation.WSLgFaultEvent{Kind: kind, Retryable: true})
	}
	if p.config.RequireDisplay && !p.displayReady() {
		appendFault(simulation.WSLgFaultDisplayUnavailable)
	}
	if p.config.RequireSharedMemory && !isDirectory(p.config.SharedMemoryPath) {
		appendFault(simulation.WSLgFaultSharedMemoryUnavailable)
	}
	if p.config.RequireKeepalive && !p.keepaliveRunning(ctx) {
		appendFault(simulation.WSLgFaultKeepaliveMissing)
	}
	if p.config.RequireCopyModeCheck && p.copyModeRecent() {
		appendFault(simulation.WSLgFaultCopyMode)
	}
	return faults
}

func (p *Probe) displayReady() bool {
	if !isDirectory(p.config.WSLgRoot) {
		return false
	}
	display := p.environment("DISPLAY")
	wayland := p.environment("WAYLAND_DISPLAY")
	return display != "" || wayland != ""
}

func (p *Probe) environment(name string) string {
	if p.config.Environment != nil {
		return p.config.Environment[name]
	}
	return os.Getenv(name)
}

func (p *Probe) keepaliveRunning(ctx context.Context) bool {
	entries, err := os.ReadDir(p.config.ProcessRoot)
	if err != nil {
		return false
	}
	needle := []byte(p.config.KeepaliveTitle)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return false
		}
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		wire, err := os.ReadFile(filepath.Join(p.config.ProcessRoot, entry.Name(), "cmdline"))
		if err == nil && bytes.Contains(wire, needle) {
			return true
		}
	}
	return false
}

func (p *Probe) copyModeRecent() bool {
	info, err := os.Stat(p.config.WestonLogPath)
	if err != nil || info.IsDir() || p.config.Now().Sub(info.ModTime()) > p.config.CopyModeWindow {
		return false
	}
	wire, err := readTail(p.config.WestonLogPath, 64*1024)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToUpper(string(wire)), "COPY MODE")
}

func readTail(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, 0); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

var _ simulation.WSLgProbe = (*Probe)(nil)
