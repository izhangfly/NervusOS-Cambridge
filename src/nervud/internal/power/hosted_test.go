package power

import (
	"context"
	"errors"
	"testing"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
)

type recAudit struct{ events []audit.Event }

func (r *recAudit) Record(_ context.Context, ev audit.Event) { r.events = append(r.events, ev) }

func TestHostedGateMapsActionsToHookVerbs(t *testing.T) {
	cases := []struct {
		action authority.PowerAction
		verb   string
	}{
		{authority.PowerActionPowerOff, "poweroff"},
		{authority.PowerActionReboot, "reboot"},
	}
	for _, tc := range cases {
		aud := &recAudit{}
		g := NewHostedGate("/usr/libexec/nervus/hosted-power", aud, nil)
		var gotName string
		var gotArgs []string
		g.start = func(name string, args ...string) error {
			gotName, gotArgs = name, args
			return nil
		}
		err := g.Power(context.Background(), authority.Subject{PackageID: "nervus.settings"},
			authority.PowerRequest{Action: tc.action, Reason: "test"})
		if err != nil {
			t.Fatalf("%s: unexpected error %v", tc.verb, err)
		}
		if gotName != "/usr/libexec/nervus/hosted-power" || len(gotArgs) != 1 || gotArgs[0] != tc.verb {
			t.Fatalf("%s: hook called as %q %v", tc.verb, gotName, gotArgs)
		}
		if len(aud.events) != 1 || aud.events[0].Action != "PowerAction.hosted" {
			t.Fatalf("%s: expected one audit event, got %+v", tc.verb, aud.events)
		}
	}
}

func TestHostedGateRejectsUnknownAction(t *testing.T) {
	g := NewHostedGate("/bin/true", nil, nil)
	called := false
	g.start = func(string, ...string) error { called = true; return nil }
	if err := g.Power(context.Background(), authority.Subject{}, authority.PowerRequest{}); err == nil {
		t.Fatal("expected error for unspecified action")
	}
	if called {
		t.Fatal("hook must not run for an unknown action")
	}
}

func TestHostedGateReportsHookStartFailure(t *testing.T) {
	g := NewHostedGate("/nonexistent", nil, nil)
	g.start = func(string, ...string) error { return errors.New("boom") }
	err := g.Power(context.Background(), authority.Subject{},
		authority.PowerRequest{Action: authority.PowerActionPowerOff, Reason: "x"})
	if err == nil {
		t.Fatal("expected hook start failure to surface")
	}
}
