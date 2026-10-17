//go:build linux

package provider

import "golang.org/x/sys/unix"

func monotonicNowNanos() (int64, error) {
	var value unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &value); err != nil {
		return 0, err
	}
	return value.Sec*1_000_000_000 + value.Nsec, nil
}
