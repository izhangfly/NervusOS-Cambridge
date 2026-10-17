package simprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

const defaultStopGrace = 5 * time.Second

type Config struct {
	Executable       string
	BaseArgs         []string
	Worlds           map[string]string
	WorldRoots       []string
	WorkingDirectory string
	Environment      []string
	StopGrace        time.Duration
	Stdout           io.Writer
	Stderr           io.Writer
}

type managedProcess struct {
	cmd      *exec.Cmd
	identity processIdentity
	done     chan struct{}
	expected bool
	world    string
}

type Managed struct {
	config Config
	worlds map[string]string
	names  []string

	mu        sync.Mutex
	current   *managedProcess
	snapshot  simulation.ProcessSnapshot
	events    chan simulation.ProcessEvent
	closed    bool
	closeOnce sync.Once
	waiters   sync.WaitGroup
	closeErr  error
}

func NewManaged(config Config) (*Managed, error) {
	normalized, worlds, names, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return &Managed{
		config:   normalized,
		worlds:   worlds,
		names:    names,
		snapshot: simulation.ProcessSnapshot{State: simulation.ProcessStopped},
		events:   make(chan simulation.ProcessEvent, 16),
	}, nil
}

func (m *Managed) Mode() simulation.Mode { return simulation.ModeManaged }

func (m *Managed) AllowedWorlds() []string { return append([]string(nil), m.names...) }

func (m *Managed) Snapshot() simulation.ProcessSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot
}

func (m *Managed) Start(ctx context.Context, world string) (simulation.ProcessSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return simulation.ProcessSnapshot{}, err
	}
	path, ok := m.worlds[world]
	if !ok {
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonWorldNotFound, "simprocess.managed.start", false,
		)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonInvalidState, "simprocess.managed.start", false,
		)
	}
	if m.current != nil {
		m.mu.Unlock()
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonProcessAlreadyRunning, "simprocess.managed.start", false,
		)
	}
	m.snapshot.State = simulation.ProcessStarting
	m.snapshot.World = world
	m.snapshot.Owned = false
	m.snapshot.ProcessID = 0
	m.mu.Unlock()

	args := append(append([]string(nil), m.config.BaseArgs...), path)
	cmd := exec.Command(m.config.Executable, args...)
	cmd.Dir = m.config.WorkingDirectory
	cmd.Env = append(os.Environ(), m.config.Environment...)
	cmd.Stdout = m.config.Stdout
	cmd.Stderr = m.config.Stderr
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		m.mu.Lock()
		m.snapshot.State = simulation.ProcessStopped
		m.snapshot.World = ""
		m.mu.Unlock()
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonNativeFailure, "simprocess.managed.start", true,
		)
	}
	identity, err := identifyProcess(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		m.mu.Lock()
		m.snapshot.State = simulation.ProcessStopped
		m.snapshot.World = ""
		m.mu.Unlock()
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonNativeFailure, "simprocess.managed.identity", true,
		)
	}
	owned := &managedProcess{
		cmd: cmd, identity: identity, done: make(chan struct{}), world: world,
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = signalProcessGroup(identity, true)
		_, _ = cmd.Process.Wait()
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonInvalidState, "simprocess.managed.start", false,
		)
	}
	m.current = owned
	m.snapshot = simulation.ProcessSnapshot{
		State: simulation.ProcessRunning, Owned: true,
		ProcessID: uint64(identity.pid), World: world,
	}
	snapshot := m.snapshot
	m.mu.Unlock()
	m.waiters.Add(1)
	go m.wait(owned)
	return snapshot, nil
}

func (m *Managed) Stop(ctx context.Context) (simulation.ProcessSnapshot, error) {
	m.mu.Lock()
	owned := m.current
	if owned == nil {
		m.mu.Unlock()
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonProcessNotRunning, "simprocess.managed.stop", false,
		)
	}
	owned.expected = true
	m.snapshot.State = simulation.ProcessStopping
	done := owned.done
	identity := owned.identity
	m.mu.Unlock()

	if err := signalProcessGroup(identity, false); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonNativeFailure, "simprocess.managed.stop", true,
		)
	}
	timer := time.NewTimer(m.config.StopGrace)
	defer timer.Stop()
	select {
	case <-done:
		return m.Snapshot(), nil
	case <-ctx.Done():
		return simulation.ProcessSnapshot{}, ctx.Err()
	case <-timer.C:
	}
	if err := signalProcessGroup(identity, true); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return simulation.ProcessSnapshot{}, simulation.NewError(
			simulation.ReasonNativeFailure, "simprocess.managed.kill", true,
		)
	}
	select {
	case <-done:
		return m.Snapshot(), nil
	case <-ctx.Done():
		return simulation.ProcessSnapshot{}, ctx.Err()
	}
}

func (m *Managed) Events() <-chan simulation.ProcessEvent { return m.events }

func (m *Managed) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		running := m.current != nil
		m.mu.Unlock()
		if running {
			ctx, cancel := context.WithTimeout(context.Background(), m.config.StopGrace+time.Second)
			_, m.closeErr = m.Stop(ctx)
			cancel()
		}
		m.mu.Lock()
		stopped := m.current == nil
		m.mu.Unlock()
		if stopped {
			m.waiters.Wait()
			close(m.events)
		}
	})
	return m.closeErr
}

func (m *Managed) wait(owned *managedProcess) {
	defer m.waiters.Done()
	err := owned.cmd.Wait()
	exitCode := int32(0)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exitCode = int32(exit.ExitCode())
		} else {
			exitCode = -1
		}
	}
	m.mu.Lock()
	if m.current != owned {
		m.mu.Unlock()
		close(owned.done)
		return
	}
	expected := owned.expected
	m.current = nil
	m.snapshot.State = simulation.ProcessExited
	if expected {
		m.snapshot.State = simulation.ProcessStopped
	}
	m.snapshot.Owned = false
	m.snapshot.ProcessID = 0
	m.snapshot.HasLastExitCode = true
	m.snapshot.LastExitCode = exitCode
	snapshot := m.snapshot
	m.mu.Unlock()
	close(owned.done)
	event := simulation.ProcessEvent{
		Snapshot: snapshot,
		HasExit:  true,
		Exit: simulation.ProcessExitEvent{
			ProcessID: uint64(owned.identity.pid), ExitCode: exitCode, Expected: expected,
		},
	}
	select {
	case m.events <- event:
	default:
	}
}

func normalizeConfig(config Config) (Config, map[string]string, []string, error) {
	if !managedSupported() {
		return Config{}, nil, nil, errors.New("simprocess: managed mode requires Linux")
	}
	if config.Executable == "" {
		return Config{}, nil, nil, errors.New("simprocess: executable is required")
	}
	executable, err := exec.LookPath(config.Executable)
	if err != nil {
		return Config{}, nil, nil, fmt.Errorf("simprocess: executable: %w", err)
	}
	config.Executable, err = filepath.Abs(executable)
	if err != nil {
		return Config{}, nil, nil, fmt.Errorf("simprocess: executable path: %w", err)
	}
	if config.StopGrace == 0 {
		config.StopGrace = defaultStopGrace
	}
	if config.StopGrace < 10*time.Millisecond || config.StopGrace > time.Minute {
		return Config{}, nil, nil, errors.New("simprocess: stop grace is out of range")
	}
	if len(config.WorldRoots) == 0 || len(config.Worlds) == 0 {
		return Config{}, nil, nil, errors.New("simprocess: world roots and worlds are required")
	}
	roots := make([]string, 0, len(config.WorldRoots))
	for _, root := range config.WorldRoots {
		canonical, err := canonicalPath(root)
		if err != nil {
			return Config{}, nil, nil, fmt.Errorf("simprocess: world root: %w", err)
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			return Config{}, nil, nil, errors.New("simprocess: world root is not a directory")
		}
		roots = append(roots, canonical)
	}
	worlds := make(map[string]string, len(config.Worlds))
	names := make([]string, 0, len(config.Worlds))
	for name, path := range config.Worlds {
		if !validWorldName(name) {
			return Config{}, nil, nil, errors.New("simprocess: invalid world name")
		}
		canonical, err := canonicalPath(path)
		if err != nil {
			return Config{}, nil, nil, fmt.Errorf("simprocess: world path: %w", err)
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(canonical), ".sdf") {
			return Config{}, nil, nil, errors.New("simprocess: world must be a regular SDF file")
		}
		allowed := false
		for _, root := range roots {
			relative, relErr := filepath.Rel(root, canonical)
			if relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
				allowed = true
				break
			}
		}
		if !allowed {
			return Config{}, nil, nil, errors.New("simprocess: world is outside allowed roots")
		}
		worlds[name] = canonical
		names = append(names, name)
	}
	sort.Strings(names)
	if config.Stdout == nil {
		config.Stdout = os.Stdout
	}
	if config.Stderr == nil {
		config.Stderr = os.Stderr
	}
	return config, worlds, names, nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Clean(absolute))
}

func validWorldName(value string) bool {
	if value == "" || len(value) > 255 || strings.Contains(value, "..") {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("_-.:", char) {
			continue
		}
		return false
	}
	return true
}

var _ simulation.ProcessController = (*Managed)(nil)
