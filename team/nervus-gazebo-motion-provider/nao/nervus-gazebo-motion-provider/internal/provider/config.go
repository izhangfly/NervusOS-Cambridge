package provider

import (
	"fmt"
	"math"
	"time"
)

const (
	defaultEndpointTimeout = 5 * time.Second
	defaultShutdownTimeout = 10 * time.Second
	defaultMaxStepCount    = 10000
	defaultMaxJointBatch   = 64
	defaultMaxNameBytes    = 255
)

type Config struct {
	EndpointTimeout time.Duration
	ShutdownTimeout time.Duration
	MaxStepCount    uint32
	MaxJointBatch   int
	MaxNameBytes    int
	MinJointRadians float64
	MaxJointRadians float64
}

func DefaultConfig() Config {
	return Config{
		EndpointTimeout: defaultEndpointTimeout,
		ShutdownTimeout: defaultShutdownTimeout,
		MaxStepCount:    defaultMaxStepCount,
		MaxJointBatch:   defaultMaxJointBatch,
		MaxNameBytes:    defaultMaxNameBytes,
		MinJointRadians: -2 * math.Pi,
		MaxJointRadians: 2 * math.Pi,
	}
}

func (c Config) normalize() (Config, error) {
	defaults := DefaultConfig()
	if c.EndpointTimeout == 0 {
		c.EndpointTimeout = defaults.EndpointTimeout
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = defaults.ShutdownTimeout
	}
	if c.MaxStepCount == 0 {
		c.MaxStepCount = defaults.MaxStepCount
	}
	if c.MaxJointBatch == 0 {
		c.MaxJointBatch = defaults.MaxJointBatch
	}
	if c.MaxNameBytes == 0 {
		c.MaxNameBytes = defaults.MaxNameBytes
	}
	if c.MinJointRadians == 0 && c.MaxJointRadians == 0 {
		c.MinJointRadians = defaults.MinJointRadians
		c.MaxJointRadians = defaults.MaxJointRadians
	}

	if c.EndpointTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return Config{}, fmt.Errorf("provider: endpoint and shutdown timeouts must be positive")
	}
	if c.MaxStepCount < 1 || c.MaxStepCount > 1_000_000 {
		return Config{}, fmt.Errorf("provider: max step count must be between 1 and 1000000")
	}
	if c.MaxJointBatch < 1 || c.MaxJointBatch > 4096 {
		return Config{}, fmt.Errorf("provider: max joint batch must be between 1 and 4096")
	}
	if c.MaxNameBytes < 1 || c.MaxNameBytes > 4096 {
		return Config{}, fmt.Errorf("provider: max name bytes must be between 1 and 4096")
	}
	if math.IsNaN(c.MinJointRadians) || math.IsNaN(c.MaxJointRadians) ||
		math.IsInf(c.MinJointRadians, 0) || math.IsInf(c.MaxJointRadians, 0) ||
		c.MinJointRadians >= c.MaxJointRadians {
		return Config{}, fmt.Errorf("provider: invalid joint command range")
	}
	return c, nil
}
