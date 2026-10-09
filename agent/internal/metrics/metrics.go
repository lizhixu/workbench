// Package metrics implements system resource sampling on the agent.
// Uses gopsutil for cross-platform CPU/mem/disk/net collection.
package metrics

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"watchman/proto/agentpb"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
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

	// Host boot time (unix seconds), cached at startup. It only changes
	// across a host reboot — which also restarts this process — so it is a
	// stable reboot detector for the traffic tracker.
	bootTime int64

	// Previous cumulative counters for computing per-second rates.
	prevNetRx     float64
	prevNetTx     float64
	prevDiskRead  float64
	prevDiskWrite float64
	prevTs        time.Time

	// Monthly traffic accounting (persisted next to the agent state file).
	traffic *trafficTracker

	// Network quality probe (latency/loss vs a panel-configured target).
	prober *prober

	// CPU model is static — fetched once and cached.
	cpuModelOnce sync.Once
	cpuModel     string
}

func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{log: log, prober: newProber()}
	if hi, err := host.Info(); err == nil {
		m.bootTime = int64(hi.BootTime)
	}
	return m
}

// SetStateFile wires the monthly traffic tracker to persist alongside the
// agent identity. resetDay is the billing-cycle reset day-of-month (1..28);
// allowIfaces optionally restricts accounting to the named interfaces
// (empty = all non-loopback interfaces).
func (m *Manager) SetStateFile(stateFile string, resetDay int, allowIfaces []string) {
	m.traffic = newTrafficTracker(stateFile, resetDay, allowIfaces)
}

// SetResetDay applies the panel-configured billing reset day delivered by
// the server in RegisterResponse; values outside 1..28 are ignored so an
// old server (or a transient 0) never clobbers the local setting.
func (m *Manager) SetResetDay(day int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.traffic != nil {
		m.traffic.setResetDay(day)
	}
}

// SetProbeURL applies the probe target delivered by the server in
// RegisterResponse (already resolved to the effective URL, default
// included). Empty disables probing (old server).
func (m *Manager) SetProbeURL(url string) {
	if m.prober != nil {
		m.prober.setURL(url)
	}
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

// StartBackground begins periodic metrics sampling at the given interval
// (seconds). This runs automatically when the agent connects so the server
// always has fresh lastMetrics (including uptime) for the host list API,
// without waiting for the browser to open the monitoring page.
func (m *Manager) StartBackground(intervalSec int32) {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return // already running (e.g. live mode triggered by frontend)
	}
	m.mu.Unlock()
	m.startLive(intervalSec)
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

	// Swap usage.
	if sw, err := mem.SwapMemory(); err == nil {
		sample.SwapTotal = int64(sw.Total)
		sample.SwapUsed = int64(sw.Used)
	}

	// Load averages (Linux/macOS; unsupported on Windows → left at 0).
	if lv, err := load.Avg(); err == nil {
		sample.Load1 = lv.Load1
		sample.Load5 = lv.Load5
		sample.Load15 = lv.Load15
	}

	// Total process count.
	if pids, err := process.Pids(); err == nil {
		sample.ProcessCount = int32(len(pids))
	}

	// TCP ESTABLISHED + UDP socket counts.
	if conns, err := net.Connections("tcp"); err == nil {
		n := int32(0)
		for _, c := range conns {
			if c.Status == "ESTABLISHED" {
				n++
			}
		}
		sample.TcpEstablished = n
	}
	if conns, err := net.Connections("udp"); err == nil {
		sample.UdpCount = int32(len(conns))
	}

	// CPU model name (static, cached after first lookup).
	sample.CpuModel = m.cpuModelString()

	// Network quality probe (latest completed round; runs off-thread so
	// collection never waits on the network).
	if m.prober != nil {
		if r := m.prober.latest(); r.done {
			sample.NetLatencyMs = r.latencyMs
			sample.NetLossPct = r.lossPct
		}
	}

	// Network: per-interface cumulative counters. Loopback is excluded
	// (host-internal traffic no ISP bills); an optional allowlist can
	// restrict accounting to chosen interfaces (e.g. the external NIC on
	// docker hosts, where one packet is counted on veth + bridge + NIC).
	if io, err := net.IOCounters(true); err == nil && len(io) > 0 {
		samples := make(map[string]ifaceCounters, len(io))
		var curRx, curTx uint64
		for _, nic := range io {
			if isLoopbackIface(nic.Name) {
				continue
			}
			if m.traffic != nil && !m.traffic.ifaceAllowed(nic.Name) {
				continue
			}
			curRx += nic.BytesRecv
			curTx += nic.BytesSent
			samples[nic.Name] = ifaceCounters{Rx: nic.BytesRecv, Tx: nic.BytesSent}
		}
		fRx, fTx := float64(curRx), float64(curTx)
		m.mu.Lock()
		if !m.prevTs.IsZero() {
			elapsed := now.Sub(m.prevTs).Seconds()
			if elapsed > 0 {
				sample.NetRx = max64(fRx-m.prevNetRx, 0) / elapsed
				sample.NetTx = max64(fTx-m.prevNetTx, 0) / elapsed
			}
		}
		m.prevNetRx = fRx
		m.prevNetTx = fTx
		m.mu.Unlock()

		// Monthly traffic accounting from the same per-interface counters.
		if m.traffic != nil {
			m.mu.Lock()
			rx, tx := m.traffic.account(samples, m.bootTime, now)
			m.mu.Unlock()
			sample.MonthRx = int64(rx)
			sample.MonthTx = int64(tx)
		}
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

// cpuModelString returns the CPU model name (cached after the first call;
// empty string when unavailable).
func (m *Manager) cpuModelString() string {
	m.cpuModelOnce.Do(func() {
		if infos, err := cpu.Info(); err == nil && len(infos) > 0 {
			model := strings.TrimSpace(infos[0].ModelName)
			// Collapse vendor-prefix whitespace variants like "AMD Ryzen 7 5800X 8-Core Processor".
			if model != "" {
				m.cpuModel = model
			}
		}
	})
	return m.cpuModel
}