package simprocess

import (
	"context"
	"errors"
	"testing"

	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

func TestAttachNeverControlsExternalProcess(t *testing.T) {
	controller := NewAttach([]string{"nervus_home", "nervus_home"})
	if got := controller.AllowedWorlds(); len(got) != 1 || got[0] != "nervus_home" {
		t.Fatalf("AllowedWorlds = %v", got)
	}
	_, err := controller.Start(context.Background(), "nervus_home")
	var domain *simulation.Error
	if !errors.As(err, &domain) || domain.Reason != simulation.ReasonProcessExternallyManaged {
		t.Fatalf("Start error = %v", err)
	}
	_, err = controller.Stop(context.Background())
	if !errors.As(err, &domain) || domain.Reason != simulation.ReasonProcessExternallyManaged {
		t.Fatalf("Stop error = %v", err)
	}
}
