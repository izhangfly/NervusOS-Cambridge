// Package jetty implements the Gazebo Jetty simulation.Backend on top of the
// in-process gztransport adapter.
package jetty

import (
	"fmt"
	"strings"
	"time"

	profilev1 "github.com/nervus-os/nervus-gazebo-provider/gen/go/gazebo/simulation/motion/profile/v1"
	"github.com/nervus-os/nervus-gazebo-provider/internal/motionprofile"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

const (
	defaultWorld                  = "nervus_home"
	defaultDiscoveryTimeout       = 5 * time.Second
	defaultDiscoveryRetryInterval = 250 * time.Millisecond
	defaultRequestTimeout         = 5 * time.Second
	defaultStatsStaleTimeout      = 2 * time.Second
	defaultEventBuffer            = 128
	defaultMaxStepIterations      = 10000
	defaultMaxJointCommands       = 64
	defaultWSLgCheckInterval      = 2 * time.Second
)

type Config struct {
	Mode                   simulation.Mode
	World                  string
	Profiles               *motionprofile.Catalog
	Runtime                *profilev1.RuntimeConfiguration
	DiscoveryTimeout       time.Duration
	DiscoveryRetryInterval time.Duration
	RequestTimeout         time.Duration
	StatsStaleTimeout      time.Duration
	EventBuffer            int
	MaxStepIterations      uint32
	MaxJointCommands       uint32
	WSLgCheckInterval      time.Duration
}

func DefaultConfig() Config {
	return Config{
		Mode:                   simulation.ModeAttach,
		World:                  defaultWorld,
		Profiles:               motionprofile.Empty(),
		DiscoveryTimeout:       defaultDiscoveryTimeout,
		DiscoveryRetryInterval: defaultDiscoveryRetryInterval,
		RequestTimeout:         defaultRequestTimeout,
		StatsStaleTimeout:      defaultStatsStaleTimeout,
		EventBuffer:            defaultEventBuffer,
		MaxStepIterations:      defaultMaxStepIterations,
		MaxJointCommands:       defaultMaxJointCommands,
		WSLgCheckInterval:      defaultWSLgCheckInterval,
	}
}

func (c Config) normalize() (Config, error) {
	defaults := DefaultConfig()
	if c.Mode == simulation.ModeUnspecified {
		c.Mode = defaults.Mode
	}
	if c.World == "" {
		c.World = defaults.World
	}
	if c.Profiles == nil {
		c.Profiles = defaults.Profiles
	}
	if c.DiscoveryTimeout == 0 {
		c.DiscoveryTimeout = defaults.DiscoveryTimeout
	}
	if c.DiscoveryRetryInterval == 0 {
		c.DiscoveryRetryInterval = defaults.DiscoveryRetryInterval
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = defaults.RequestTimeout
	}
	if c.StatsStaleTimeout == 0 {
		c.StatsStaleTimeout = defaults.StatsStaleTimeout
	}
	if c.EventBuffer == 0 {
		c.EventBuffer = defaults.EventBuffer
	}
	if c.MaxStepIterations == 0 {
		c.MaxStepIterations = defaults.MaxStepIterations
	}
	if c.MaxJointCommands == 0 {
		c.MaxJointCommands = defaults.MaxJointCommands
	}
	if c.WSLgCheckInterval == 0 {
		c.WSLgCheckInterval = defaults.WSLgCheckInterval
	}

	if c.Mode != simulation.ModeAttach && c.Mode != simulation.ModeManaged {
		return Config{}, fmt.Errorf("jetty: mode must be attach or managed")
	}
	if !validName(c.World) {
		return Config{}, fmt.Errorf("jetty: world must be a safe Gazebo name")
	}
	if c.DiscoveryTimeout <= 0 || c.DiscoveryRetryInterval <= 0 ||
		c.RequestTimeout <= 0 || c.StatsStaleTimeout <= 0 {
		return Config{}, fmt.Errorf("jetty: timeouts and retry interval must be positive")
	}
	if c.EventBuffer < 1 || c.EventBuffer > 65536 {
		return Config{}, fmt.Errorf("jetty: event buffer must be between 1 and 65536")
	}
	if c.MaxStepIterations < 1 || c.MaxJointCommands < 1 {
		return Config{}, fmt.Errorf("jetty: operation limits must be positive")
	}
	if c.WSLgCheckInterval <= 0 {
		return Config{}, fmt.Errorf("jetty: WSLg check interval must be positive")
	}
	return c, nil
}

func validName(value string) bool {
	if value == "" || len(value) > 255 || strings.Contains(value, "..") {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}
