package wslg

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func TestProbeReportsOnlyNewlyObservedEnvironmentFacts(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	wslgRoot := filepath.Join(root, "wslg")
	shared := filepath.Join(root, "shared")
	for _, path := range []string{proc, wslgRoot, shared, filepath.Join(proc, "123")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(proc, "123", "cmdline"),
		[]byte("xmessage\x00-title\x00NervusWSLgKeepalive\x00"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	weston := filepath.Join(wslgRoot, "weston.log")
	if err := os.WriteFile(weston, []byte("renderer entered COPY MODE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(weston, now, now); err != nil {
		t.Fatal(err)
	}
	probe, err := New(Config{
		RequireDisplay: true, RequireKeepalive: true, RequireSharedMemory: true,
		RequireCopyModeCheck: true,
		ProcessRoot:          proc, WSLgRoot: wslgRoot, SharedMemoryPath: shared,
		WestonLogPath: weston, KeepaliveTitle: "NervusWSLgKeepalive",
		CopyModeWindow: time.Minute, Environment: map[string]string{"DISPLAY": ":0"},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	faults := probe.Check(context.Background())
	if len(faults) != 1 || faults[0].Kind != simulation.WSLgFaultCopyMode {
		t.Fatalf("faults = %+v, want COPY_MODE", faults)
	}
}

func TestProbeReportsMissingDisplaySharedMemoryAndKeepalive(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	if err := os.Mkdir(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	probe, err := New(Config{
		RequireDisplay: true, RequireKeepalive: true, RequireSharedMemory: true,
		RequireCopyModeCheck: true,
		ProcessRoot:          proc, WSLgRoot: filepath.Join(root, "missing-wslg"),
		SharedMemoryPath: filepath.Join(root, "missing-shared"),
		WestonLogPath:    filepath.Join(root, "missing-weston.log"),
		Environment:      map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	faults := probe.Check(context.Background())
	want := map[simulation.WSLgFaultKind]bool{
		simulation.WSLgFaultDisplayUnavailable:      true,
		simulation.WSLgFaultSharedMemoryUnavailable: true,
		simulation.WSLgFaultKeepaliveMissing:        true,
	}
	if len(faults) != len(want) {
		t.Fatalf("faults = %+v", faults)
	}
	for _, fault := range faults {
		if !want[fault.Kind] {
			t.Fatalf("unexpected fault: %+v", fault)
		}
	}
}
