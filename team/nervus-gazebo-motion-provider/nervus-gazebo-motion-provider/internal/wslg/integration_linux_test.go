//go:build linux && wslgintegration

package wslg

import (
	"context"
	"testing"
)

func TestCurrentNervusWSLgLauncherEnvironment(t *testing.T) {
	probe, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if faults := probe.Check(context.Background()); len(faults) != 0 {
		t.Fatalf("current launcher-owned WSLg environment faults: %+v", faults)
	}
}
