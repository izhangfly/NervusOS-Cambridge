//go:build !linux

package provider

import "errors"

func monotonicNowNanos() (int64, error) {
	return 0, errors.New("provider: Linux CLOCK_MONOTONIC is unavailable")
}
