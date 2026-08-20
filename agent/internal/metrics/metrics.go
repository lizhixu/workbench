// Package metrics implements system resource sampling on the agent.
// Uses gopsutil for cross-platform CPU/mem/disk/net collection.
package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"watchman/proto/agentpb"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
)

// Sender pushes agent->server messages.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// Manager handles metrics sampling.
type Manager struct {
	mu     sync.Mutex
	sender Sender
	log    *slog.Logger
	cancel context.CancelFunc

	// Previous cumulative counters for computing per-second rates.
	prevNetRx    float64
	prevNetTx    float64
	prevDiskRead float64
	prevDiskWrite float64
	prevTs       time.Time
}

func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log}
}

func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	m.sender = s
	m.mu.Unlock()
}

func (m *Manager) Handle(q *agentpb.MetricsQuery) {
	if q.GetLive() {
		m.startLive(q.GetIntervalSec())
	} else {
		m.sendSample()
	}
}

func (m *Manager) startLive(intervalSec int32) {
	m.Stop()
	if intervalSec <= 0 {
		intervalSec = 5
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()

	go func() {
		// Send immediately, then on interval.
		m.sendSample()
		t := time.NewTicker(time.Duration(intervalSec) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.sendSample()
			}
		}
	}()
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

func (m *Manager) sendSample() {
	sample := m.collect()
	if sample == nil {
		return
	}
	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s != nil {
		s.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_Metrics{Metrics: sample},
		})
	}
}

func (m *Manager) collect() *agentpb.MetricsSample {
	sample := &agentpb.MetricsSample{Ts: time.Now().Unix()}
	now := time.Now()

	// CPU usage (first call returns 0 / per-call; use percent over 1s window).
	if percents, err := cpu.Percent(time.Second, false); err == nil && len(percents) > 0 {
		sample.CpuUsage = percents[0]
	}

	// Real OS uptime (seconds since boot), independent of agent process lifetime.
	if up, err := host.Uptime(); err == nil {
		sample.Uptime = int64(up)
	}

	// Memory.
	if vm, err := mem.VirtualMemory(); err == nil {
		sample.MemUsage = vm.UsedPercent
		sample.MemTotal = int64(vm.Total)
		sample.MemUsed = int64(vm.Used)
	}

	// Network: gopsutil returns cumulative counters; compute per-second rates.
	if io, err := net.IOCounters(false); err == nil && len(io) > 0 {
		curRx := float64(io[0].BytesRecv)
		curTx := float64(io[0].BytesSent)
		m.mu.Lock()
		if !m.prevTs.IsZero() {
			elapsed := now.Sub(m.prevTs).Seconds()
			if elapsed > 0 {
				sample.NetRx = max64(curRx-m.prevNetRx, 0) / elapsed
				sample.NetTx = max64(curTx-m.prevNetTx, 0) / elapsed
			}
		}
		m.prevNetRx = curRx
		m.prevNetTx = curTx
		m.mu.Unlock()
	}

	// Disk IO: same — cumulative counters converted to per-second rates.
	if io, err := disk.IOCounters(); err == nil {
		var read, write uint64
		for _, d := range io {
			read += d.ReadBytes
			write += d.WriteBytes
		}
		curRead := float64(read)
		curWrite := float64(write)
		m.mu.Lock()
		if !m.prevTs.IsZero() {
			elapsed := now.Sub(m.prevTs).Seconds()
			if elapsed > 0 {
				sample.DiskRead = max64(curRead-m.prevDiskRead, 0) / elapsed
				sample.DiskWrite = max64(curWrite-m.prevDiskWrite, 0) / elapsed
			}
		}
		m.prevDiskRead = curRead
		m.prevDiskWrite = curWrite
		m.prevTs = now
		m.mu.Unlock()
	}

	// Mounts / filesystem usage.
	if parts, err := disk.Partitions(false); err == nil {
		for _, p := range parts {
			u, err := disk.Usage(p.Mountpoint)
			if err != nil {
				continue
			}
			sample.Mounts = append(sample.Mounts, &agentpb.Mount{
				Path:  p.Mountpoint,
				Total: int64(u.Total),
				Used:  int64(u.Used),
			})
		}
	}

	return sample
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}