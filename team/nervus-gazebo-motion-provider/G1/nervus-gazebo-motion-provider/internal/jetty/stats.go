package jetty

import (
	"math"
	"time"

	"github.com/nervus-os/nervus-gazebo-provider/internal/gzmsgs"
	"github.com/nervus-os/nervus-gazebo-provider/internal/simulation"
	"google.golang.org/protobuf/proto"
)

type decodedStats struct {
	statistics simulation.Statistics
	run        simulation.RunState
}

func decodeStats(payload []byte) (decodedStats, error) {
	var message gzmsgs.WorldStatistics
	if err := proto.Unmarshal(payload, &message); err != nil {
		return decodedStats{}, simulation.NewError(
			simulation.ReasonNativeFailure,
			"jetty.decode_stats",
			false,
		)
	}
	simTime, simValid := timeNanos(message.GetSimTime())
	realTime, realValid := timeNanos(message.GetRealTime())
	factor := message.GetRealTimeFactor()
	if math.IsNaN(factor) || math.IsInf(factor, 0) {
		factor = 0
		realValid = false
	}
	run := simulation.RunRunning
	if message.GetPaused() {
		run = simulation.RunPaused
	}
	return decodedStats{
		statistics: simulation.Statistics{
			Valid:            simValid && realValid,
			SimulationTimeNS: simTime,
			RealTimeNS:       realTime,
			Iterations:       message.GetIterations(),
			RealTimeFactor:   factor,
		},
		run: run,
	}, nil
}

func timeNanos(value *gzmsgs.Time) (int64, bool) {
	if value == nil || value.GetSec() < 0 || value.GetNsec() < 0 ||
		value.GetNsec() >= 1_000_000_000 || value.GetSec() > math.MaxInt64/1_000_000_000 {
		return 0, false
	}
	return value.GetSec()*1_000_000_000 + int64(value.GetNsec()), true
}

func (b *Backend) applyStats(stats decodedStats) {
	b.mu.Lock()
	b.status.Run = stats.run
	b.status.Statistics = stats.statistics
	b.status.LastFault = simulation.ReasonUnspecified
	b.lastStatsAt = time.Now()
	b.statsSequence++
	b.notifyLocked()
	status := b.status
	b.mu.Unlock()
	b.emitStatus(status)
}
