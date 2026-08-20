package ai

import (
	"watchman/server/internal/metrics"
)

// metricsStoreAdapter wraps *metrics.Store to satisfy ai.metricsStoreRef.
type metricsStoreAdapter struct {
	store *metrics.Store
}

// NewMetricsStoreAdapter wraps a metrics.Store so the AI assistant can query
// historical metrics without importing the metrics package directly.
func NewMetricsStoreAdapter(s *metrics.Store) metricsStoreRef {
	return &metricsStoreAdapter{store: s}
}

func (a *metricsStoreAdapter) QueryHistory(hostID string, from, to int64, stepSec int) []metricsPoint {
	pts := a.store.QueryHistory(hostID, from, to, stepSec)
	out := make([]metricsPoint, len(pts))
	for i, p := range pts {
		out[i] = metricsPoint{
			Timestamp: p.Timestamp,
			CPUUsage:  p.CPUUsage,
			MemUsage:  p.MemUsage,
			MemTotal:  p.MemTotal,
			MemUsed:   p.MemUsed,
			NetRx:     p.NetRx,
			NetTx:     p.NetTx,
			DiskRead:  p.DiskRead,
			DiskWrite: p.DiskWrite,
		}
	}
	return out
}