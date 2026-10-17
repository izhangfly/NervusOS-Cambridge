//go:build linux

package simprocess

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func TestManagedOwnsAndStopsOnlyStartedProcessGroup(t *testing.T) {
	controller := newHelperController(t, "wait")
	snapshot, err := controller.Start(context.Background(), "nervus_home")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snapshot.State != simulation.ProcessRunning || !snapshot.Owned || snapshot.ProcessID == 0 {
		t.Fatalf("running snapshot = %+v", snapshot)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stopped, err := controller.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.State != simulation.ProcessStopped || stopped.Owned || !stopped.HasLastExitCode {
		t.Fatalf("stopped snapshot = %+v", stopped)
	}
	select {
	case event := <-controller.Events():
		if !event.HasExit || !event.Exit.Expected || event.Exit.ProcessID != snapshot.ProcessID {
			t.Fatalf("exit event = %+v", event)
		}
	case <-ctx.Done():
		t.Fatal("expected exit event")
	}
}

func TestManagedReportsUnexpectedExit(t *testing.T) {
	controller := newHelperController(t, "exit")
	if _, err := controller.Start(context.Background(), "nervus_home"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case event := <-controller.Events():
		if !event.HasExit || event.Exit.Expected || event.Exit.ExitCode != 7 ||
			event.Snapshot.State != simulation.ProcessExited {
			t.Fatalf("exit event = %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unexpected exit was not reported")
	}
}

func TestManagedRejectsWorldOutsideAllowedRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	world := filepath.Join(outside, "outside.sdf")
	if err := os.WriteFile(world, []byte("<sdf/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewManaged(Config{
		Executable: os.Args[0], BaseArgs: []string{"-test.run=TestManagedHelperProcess"},
		WorldRoots: []string{root}, Worlds: map[string]string{"outside": world},
	})
	if err == nil {
		t.Fatal("NewManaged accepted a world outside its allowed root")
	}
}

func newHelperController(t *testing.T, action string) *Managed {
	t.Helper()
	root := t.TempDir()
	world := filepath.Join(root, "nervus_home.sdf")
	if err := os.WriteFile(world, []byte("<sdf/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	controller, err := NewManaged(Config{
		Executable: os.Args[0],
		BaseArgs:   []string{"-test.run=TestManagedHelperProcess", "--", action},
		WorldRoots: []string{root}, Worlds: map[string]string{"nervus_home": world},
		Environment: []string{"NERVUS_GAZEBO_HELPER=1"}, StopGrace: 200 * time.Millisecond,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("NewManaged: %v", err)
	}
	t.Cleanup(func() {
		if err := controller.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return controller
}

func TestManagedHelperProcess(t *testing.T) {
	if os.Getenv("NERVUS_GAZEBO_HELPER") != "1" {
		return
	}
	args := os.Args
	action := ""
	for index, arg := range args {
		if arg == "--" && index+1 < len(args) {
			action = args[index+1]
			break
		}
	}
	if action == "exit" {
		os.Exit(7)
	}
	if action != "wait" {
		panic(errors.New("unknown helper action"))
	}
	select {}
}
