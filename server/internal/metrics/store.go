// Package metrics provides time-series storage, downsampled history querying,
// periodic sampling, and 7-day retention management for agent metrics.
package metrics

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// Point is a single time-series metric point.
type Point struct {
	Timestamp int64   `json:"ts"`
	CPUUsage  float64 `json:"cpu_usage"`
	MemUsage  float64 `json:"mem_usage"`
	MemTotal  int64   `json:"mem_total"`
	MemUsed   int64   `json:"mem_used"`
	NetRx     float64 `json:"net_rx"`
	NetTx     float64 `json:"net_tx"`
	DiskRead  float64 `json:"disk_read"`
	DiskWrite float64 `json:"disk_write"`
	// Extended metrics (older records simply have zero values).
	Load1         float64 `json:"load1"`
	Load5         float64 `json:"load5"`
	Load15        float64 `json:"load15"`
	SwapTotal     int64   `json:"swap_total"`
	SwapUsed      int64   `json:"swap_used"`
	TcpEstablished int32  `json:"tcp_established"`
	UdpCount      int32   `json:"udp_count"`
	ProcessCount  int32   `json:"process_count"`
	MonthRx       int64   `json:"month_rx"`
	MonthTx       int64   `json:"month_tx"`
	// Network quality probe (zero on records from older agents).
	NetLatencyMs float64 `json:"net_latency_ms"`
	NetLossPct   float64 `json:"net_loss_pct"`
}

// Store handles metric persistence and downsampling.
type Store struct {
	mu       sync.RWMutex
	dataDir  string
	log      *slog.Logger
	memCache map[string][]Point // host_id -> recent points
}

// NewStore initializes a metrics time-series store under dataDir/metrics.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	dir := filepath.Join(dataDir, "metrics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create metrics dir: %w", err)
	}
	return &Store{
		dataDir:  dir,
		log:      log,
		memCache: make(map[string][]Point),
	}, nil
}

// AddSample saves a metrics sample into memory cache and daily partitioned log.
func (s *Store) AddSample(hostID string, m *agentpb.MetricsSample) {
	if m == nil || hostID == "" {
		return
	}
	pt := Point{
		Timestamp: m.GetTs(),
		CPUUsage:  m.GetCpuUsage(),
		MemUsage:  m.GetMemUsage(),
		MemTotal:  m.GetMemTotal(),
		MemUsed:   m.GetMemUsed(),
		NetRx:     m.GetNetRx(),
		NetTx:     m.GetNetTx(),
		DiskRead:  m.GetDiskRead(),
		DiskWrite: m.GetDiskWrite(),
		Load1:         m.GetLoad1(),
		Load5:         m.GetLoad5(),
		Load15:        m.GetLoad15(),
		SwapTotal:     m.GetSwapTotal(),
		SwapUsed:      m.GetSwapUsed(),
		TcpEstablished: m.GetTcpEstablished(),
		UdpCount:      m.GetUdpCount(),
		ProcessCount:  m.GetProcessCount(),
		MonthRx:       m.GetMonthRx(),
		MonthTx:       m.GetMonthTx(),
		NetLatencyMs:  m.GetNetLatencyMs(),
		NetLossPct:    m.GetNetLossPct(),
	}
	if pt.Timestamp == 0 {
		pt.Timestamp = time.Now().Unix()
	}

	s.mu.Lock()
	pts := s.memCache[hostID]
	pts = append(pts, pt)
	// Keep up to 2000 points in memory cache (~8 hours of 15s samples)
	if len(pts) > 2000 {
		pts = pts[len(pts)-2000:]
	}
	s.memCache[hostID] = pts
	s.mu.Unlock()

	// Append to daily partitioned file asynchronously or inline
	go s.appendToFile(hostID, pt)
}

func (s *Store) appendToFile(hostID string, pt Point) {
	hostDir := filepath.Join(s.dataDir, hostID)
	_ = os.MkdirAll(hostDir, 0o755)

	dayStr := time.Unix(pt.Timestamp, 0).UTC().Format("2006-01-02")
	filePath := filepath.Join(hostDir, dayStr+".jsonl")

	data, err := json.Marshal(pt)
	if err != nil {
		return
	}
	data = append(data, '\n')

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		s.log.Error("open metrics log", "host", hostID, "err", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(data)
}

// QueryHistory retrieves points in [from, to] downsampled by stepSec (seconds).
func (s *Store) QueryHistory(hostID string, from, to int64, stepSec int) []Point {
	if stepSec <= 0 {
		stepSec = 15
	}
	if to <= from {
		to = time.Now().Unix()
	}

	// 1. Gather all raw points in time range from memory cache + disk files
	raw := s.loadRawPoints(hostID, from, to)
	if len(raw) == 0 {
		return []Point{}
	}

	// Sort raw points by timestamp
	sort.Slice(raw, func(i, j int) bool {
		return raw[i].Timestamp < raw[j].Timestamp
	})

	// If step is small (e.g. <= 15s) and raw points are few, return raw directly
	if stepSec <= 15 || len(raw) <= 60 {
		return raw
	}

	// 2. Downsample points by time bucket. Rates and utilisation are averaged
	// within the bucket; capacities (mem/swap total) take the bucket max; and
	// monthly traffic counters, being monotonic, take the latest value so the
	// downsampled series still reports true month-to-date usage.
	type bucket struct {
		ts        int64
		count     int
		cpu       float64
		mem       float64
		memTotal  int64
		memUsed   int64
		netRx     float64
		netTx     float64
		diskRead  float64
		diskWrite float64
		load1     float64
		load5     float64
		load15    float64
		swapTotal int64
		swapUsed  int64
		tcpEst    float64
		udpCount  float64
		procCount float64
		monthRx   int64
		monthTx   int64
		latencySum float64
		latencyN   int
		lossSum    float64
	}

	buckets := make(map[int64]*bucket)
	var bucketKeys []int64

	for _, pt := range raw {
		bKey := (pt.Timestamp / int64(stepSec)) * int64(stepSec)
		b, ok := buckets[bKey]
		if !ok {
			b = &bucket{ts: bKey, memTotal: pt.MemTotal, swapTotal: pt.SwapTotal}
			buckets[bKey] = b
			bucketKeys = append(bucketKeys, bKey)
		}
		b.count++
		b.cpu += pt.CPUUsage
		b.mem += pt.MemUsage
		b.memUsed += pt.MemUsed
		b.netRx += pt.NetRx
		b.netTx += pt.NetTx
		b.diskRead += pt.DiskRead
		b.diskWrite += pt.DiskWrite
		b.load1 += pt.Load1
		b.load5 += pt.Load5
		b.load15 += pt.Load15
		b.swapUsed += pt.SwapUsed
		b.tcpEst += float64(pt.TcpEstablished)
		b.udpCount += float64(pt.UdpCount)
		b.procCount += float64(pt.ProcessCount)
		if pt.MemTotal > b.memTotal {
			b.memTotal = pt.MemTotal
		}
		if pt.SwapTotal > b.swapTotal {
			b.swapTotal = pt.SwapTotal
		}
		// raw is sorted ascending, so the last write wins and is the newest.
		b.monthRx = pt.MonthRx
		b.monthTx = pt.MonthTx
		// Latency averages only over samples that carry probe data
		// (0 = no data: older agent, probing off, or a fully lost round).
		if pt.NetLatencyMs > 0 {
			b.latencySum += pt.NetLatencyMs
			b.latencyN++
		}
		b.lossSum += pt.NetLossPct
	}

	sort.Slice(bucketKeys, func(i, j int) bool {
		return bucketKeys[i] < bucketKeys[j]
	})

	result := make([]Point, 0, len(bucketKeys))
	for _, k := range bucketKeys {
		b := buckets[k]
		if b.count == 0 {
			continue
		}
		n := float64(b.count)
		latency := 0.0
		if b.latencyN > 0 {
			latency = b.latencySum / float64(b.latencyN)
		}
		result = append(result, Point{
			Timestamp:      b.ts,
			CPUUsage:       b.cpu / n,
			MemUsage:       b.mem / n,
			MemTotal:       b.memTotal,
			MemUsed:        int64(float64(b.memUsed) / n),
			NetRx:          b.netRx / n,
			NetTx:          b.netTx / n,
			DiskRead:       b.diskRead / n,
			DiskWrite:      b.diskWrite / n,
			Load1:          b.load1 / n,
			Load5:          b.load5 / n,
			Load15:         b.load15 / n,
			SwapTotal:      b.swapTotal,
			SwapUsed:       int64(float64(b.swapUsed) / n),
			TcpEstablished: int32(b.tcpEst / n),
			UdpCount:       int32(b.udpCount / n),
			ProcessCount:   int32(b.procCount / n),
			MonthRx:        b.monthRx,
			MonthTx:        b.monthTx,
			NetLatencyMs:   latency,
			NetLossPct:     b.lossSum / n,
		})
	}

	return result
}

func (s *Store) loadRawPoints(hostID string, from, to int64) []Point {
	seen := make(map[int64]bool)
	var out []Point

	// Check memory cache first
	s.mu.RLock()
	for _, pt := range s.memCache[hostID] {
		if pt.Timestamp >= from && pt.Timestamp <= to {
			if !seen[pt.Timestamp] {
				seen[pt.Timestamp] = true
				out = append(out, pt)
			}
		}
	}
	s.mu.RUnlock()

	// Iterate daily files between from and to
	startDate := time.Unix(from, 0).UTC()
	endDate := time.Unix(to, 0).UTC()

	hostDir := filepath.Join(s.dataDir, hostID)
	cur := startDate
	for !cur.After(endDate) {
		dayStr := cur.Format("2006-01-02")
		fPath := filepath.Join(hostDir, dayStr+".jsonl")
		s.readDayFile(fPath, from, to, seen, &out)
		cur = cur.AddDate(0, 0, 1)
	}

	return out
}

func (s *Store) readDayFile(path string, from, to int64, seen map[int64]bool, out *[]Point) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var pt Point
		if err := json.Unmarshal(line, &pt); err == nil {
			if pt.Timestamp >= from && pt.Timestamp <= to {
				if !seen[pt.Timestamp] {
					seen[pt.Timestamp] = true
					*out = append(*out, pt)
				}
			}
		}
	}
}

// StartAutoCollector runs a background collector polling metrics from online agents every interval.
func (s *Store) StartAutoCollector(ctx context.Context, reg *rpc.Registry, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
				hosts := reg.ListAgents()
			for _, h := range hosts {
				if h.Status != "online" {
					continue
				}
				hub := reg.Hub(h.ID)
				if hub == nil {
					continue
				}
				// Query one-shot sample
				hostID := h.ID
				hub.SetRespHandler("metrics-poll", func(msg *agentpb.AgentMessage) {
					hub.SetRespHandler("metrics-poll", nil)
					if msg != nil && msg.GetMetrics() != nil {
						s.AddSample(hostID, msg.GetMetrics())
					}
				})
				hub.Send(&agentpb.ServerMessage{
					Payload: &agentpb.ServerMessage_MetricsQ{
						MetricsQ: &agentpb.MetricsQuery{Live: false},
					},
				})
			}
		}
	}
}

// StartJanitor periodically removes metrics logs older than retentionDays (e.g. 7 days).
func (s *Store) StartJanitor(ctx context.Context, retentionDays int, checkInterval time.Duration) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	s.cleanupOldFiles(retentionDays)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupOldFiles(retentionDays)
		}
	}
}

func (s *Store) cleanupOldFiles(retentionDays int) {
	if retentionDays <= 0 {
		retentionDays = 7
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)

	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return
	}

	for _, hostEntry := range entries {
		if !hostEntry.IsDir() {
			continue
		}
		hostDir := filepath.Join(s.dataDir, hostEntry.Name())
		files, err := os.ReadDir(hostDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.Type().IsRegular() || filepath.Ext(f.Name()) != ".jsonl" {
				continue
			}
			dateStr := f.Name()[:len(f.Name())-len(".jsonl")]
			fileDate, err := time.Parse("2006-01-02", dateStr)
			if err == nil && fileDate.Before(cutoff) {
				_ = os.Remove(filepath.Join(hostDir, f.Name()))
				s.log.Debug("pruned expired metrics log", "host", hostEntry.Name(), "file", f.Name())
			}
		}
	}
}
