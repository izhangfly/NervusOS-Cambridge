// Package simprocess implements the attach and managed Gazebo process modes.
// It does not know about Nervus handlers or Gazebo Transport operations.
package simprocess

import (
	"context"
	"sort"
	"sync"

	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
)

type Attach struct {
	worlds []string
	events chan simulation.ProcessEvent
	once   sync.Once
}

func NewAttach(worlds []string) *Attach {
	return &Attach{worlds: normalizeWorldNames(worlds), events: make(chan simulation.ProcessEvent)}
}

func (a *Attach) Mode() simulation.Mode { return simulation.ModeAttach }

func (a *Attach) AllowedWorlds() []string {
	return append([]string(nil), a.worlds...)
}

func (a *Attach) Snapshot() simulation.ProcessSnapshot {
	return simulation.ProcessSnapshot{State: simulation.ProcessExternal}
}

func (a *Attach) Start(context.Context, string) (simulation.ProcessSnapshot, error) {
	return simulation.ProcessSnapshot{}, externallyManaged("simprocess.attach.start")
}

func (a *Attach) Stop(context.Context) (simulation.ProcessSnapshot, error) {
	return simulation.ProcessSnapshot{}, externallyManaged("simprocess.attach.stop")
}

func (a *Attach) Events() <-chan simulation.ProcessEvent { return a.events }

func (a *Attach) Close() error {
	a.once.Do(func() { close(a.events) })
	return nil
}

func externallyManaged(operation string) error {
	return simulation.NewError(simulation.ReasonProcessExternallyManaged, operation, false)
}

func normalizeWorldNames(worlds []string) []string {
	seen := make(map[string]struct{}, len(worlds))
	for _, world := range worlds {
		if world != "" {
			seen[world] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for world := range seen {
		out = append(out, world)
	}
	sort.Strings(out)
	return out
}

var _ simulation.ProcessController = (*Attach)(nil)
