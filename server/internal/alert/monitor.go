package alert

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/ai"
	"watchman/server/internal/metrics"
	"watchman/server/internal/rpc"
)

// HostInfo is a snapshot of an agent's state used for rule evaluation.
type HostInfo struct {
	ID       string
	Hostname string
	Group    string
	Status   string // online / offline
	Metrics  *agentpb.MetricsSample
}

// HostProvider supplies the current host list to the monitor. The registry
// implements this interface.
type HostProvider interface {
	ListHosts() []HostInfo
}

// Monitor periodically evaluates alert rules against the live host state.
type Monitor struct {
	store        *Store
	provider     HostProvider
	metricsStore *metrics.Store
	aiAssistant  *ai.Assistant
	log          *slog.Logger
	stop         chan struct{}

	mu           sync.Mutex
	startTime    time.Time
	bootGrace    time.Duration
	bootstrapped bool
	prevStatus   map[string]string // hostID -> "online" | "offline"
}

// NewMonitor creates a monitor that evaluates rules every interval.
// metricsStore and aiAssistant are optional (enable anomaly detection + AI
// interpretation when non-nil).
func NewMonitor(store *Store, provider HostProvider, metricsStore *metrics.Store, aiAssistant *ai.Assistant, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	return &Monitor{
		store:        store,
		provider:     provider,
		metricsStore: metricsStore,
		aiAssistant:  aiAssistant,
		log:          log,
		stop:         make(chan struct{}),
		startTime:    time.Now(),
		bootGrace:    90 * time.Second, // aligns with reaper window (heartbeatSec 30s * heartbeatGraceFactor 3)
		prevStatus:   make(map[string]string),
	}
}

// SetBootGraceForTest overrides the cold-start suppression window in tests.
func (m *Monitor) SetBootGraceForTest(d time.Duration) {
	m.mu.Lock()
	m.bootGrace = d
	m.mu.Unlock()
}

// Start launches the monitor goroutine. It runs until Stop is called.
func (m *Monitor) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	m.log.Info("alert monitor started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-ticker.C:
			m.evaluate()
		}
	}
}

// Stop halts the monitor.
func (m *Monitor) Stop() {
	close(m.stop)
}

func (m *Monitor) evaluate() {
	// Alert storm suppression: if >20 events fired in the last 5 minutes,
	// collapse them into a single summary event and downgrade the rest.
	m.suppressStorm()

	rules := m.store.ListRules()
	hosts := m.provider.ListHosts()

	m.mu.Lock()
	firstRun := !m.bootstrapped
	isBootstrapping := firstRun || time.Since(m.startTime) < m.bootGrace
	if firstRun {
		// First evaluation on startup: establish baseline for all currently
		// known hosts. Pre-seed online firing state so already-online hosts
		// do not fire "host online" notifications.
		for _, host := range hosts {
			m.prevStatus[host.ID] = host.Status
			if host.Status == "online" {
				for _, rule := range rules {
					if rule.Type == RuleOnline {
						m.store.setFiring(rule.ID+":"+host.ID, true)
					}
				}
			}
		}
		m.bootstrapped = true
	}
	m.mu.Unlock()

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		// Cold-start suppression: during the boot grace period, do not fire
		// online notifications or offline alarms. This gives agents time to
		// reconnect after a server restart without false alarms.
		if isBootstrapping && (rule.Type == RuleOnline || rule.Type == RuleOffline) {
			continue
		}
		for _, host := range hosts {
			if !matchesFilter(host, rule) {
				continue
			}
			m.checkRule(rule, host)
		}
	}

	// Update host status tracking and seed online firing state during bootstrap.
	m.mu.Lock()
	for _, host := range hosts {
		if isBootstrapping && host.Status == "online" {
			for _, rule := range rules {
				if rule.Type == RuleOnline {
					m.store.setFiring(rule.ID+":"+host.ID, true)
				}
			}
		}
		m.prevStatus[host.ID] = host.Status
	}
	m.mu.Unlock()
}

// suppressStorm detects alert storms (>20 events in 5 min) and merges them
// into a single critical summary event, downgrading the originals to info.
func (m *Monitor) suppressStorm() {
	now := time.Now()
	windowStart := now.Add(-5 * time.Minute)

	m.store.mu.Lock()
	var recent []*Event
	for _, e := range m.store.events {
		if e.FiredAt.After(windowStart) && !e.Resolved {
			recent = append(recent, e)
		}
	}
	// Only suppress if we crossed the threshold and haven't already summarized
	// this storm (avoid re-summarizing every tick). We detect "already
	// summarized" by checking if a storm summary event exists in this window.
	alreadySummarized := false
	for _, e := range recent {
		if e.RuleID == "__storm__" {
			alreadySummarized = true
			break
		}
	}
	if len(recent) < 20 || alreadySummarized {
		m.store.mu.Unlock()
		return
	}

	// Downgrade all recent events to info severity.
	for _, e := range recent {
		e.Severity = SeverityInfo
	}
	// Build a storm summary.
	summary := &Event{
		ID:       randomID(),
		RuleID:   "__storm__",
		RuleName: "告警风暴",
		Severity: SeverityCritical,
		Message:  fmt.Sprintf("告警风暴：过去 5 分钟内触发 %d 条告警，已合并降噪", len(recent)),
		FiredAt:  now,
	}
	m.store.events = append(m.store.events, summary)
	m.store.mu.Unlock()
	_ = m.store.persist()
	m.log.Warn("alert storm suppressed", "count", len(recent))
	// AI-generate a storm summary interpretation.
	m.interpretAsync(summary)
}

func matchesFilter(host HostInfo, rule *Rule) bool {
	if rule.HostFilter != "" {
		if !contains(host.Hostname, rule.HostFilter) {
			return false
		}
	}
	if rule.GroupFilter != "" {
		if host.Group != rule.GroupFilter {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func (m *Monitor) checkRule(rule *Rule, host HostInfo) {
	key := rule.ID + ":" + host.ID
	triggered := false
	message := ""

		switch rule.Type {
		case RuleOffline:
			if host.Status != "online" {
				triggered = true
				message = fmt.Sprintf("主机 %s 已离线", host.Hostname)
			}
		case RuleOnline:
			m.mu.Lock()
			prev, hasPrev := m.prevStatus[host.ID]
			m.mu.Unlock()
			if host.Status == "online" {
				// Edge-triggered: fire ONLY if previously observed as offline,
				// or if newly discovered (hasPrev == false).
				if (hasPrev && prev != "online") || !hasPrev {
					triggered = true
					message = fmt.Sprintf("主机 %s 已上线", host.Hostname)
				} else if m.store.isFiring(key) {
					// Steady-state online: keep triggered=true so the monitor does
					// not treat it as cleared and call resolveEvent/clearFiring.
					triggered = true
					message = fmt.Sprintf("主机 %s 已上线", host.Hostname)
				}
			}
		case RuleCPUHigh:
		if host.Metrics != nil && host.Metrics.GetCpuUsage() > rule.Threshold {
			triggered = true
			message = fmt.Sprintf("主机 %s CPU 使用率 %.1f%% 超过阈值 %.0f%%", host.Hostname, host.Metrics.GetCpuUsage(), rule.Threshold)
		}
	case RuleMemHigh:
		if host.Metrics != nil {
			memPct := 0.0
			if host.Metrics.GetMemTotal() > 0 {
				memPct = float64(host.Metrics.GetMemUsed()) / float64(host.Metrics.GetMemTotal()) * 100
			}
			if memPct > rule.Threshold {
				triggered = true
				message = fmt.Sprintf("主机 %s 内存使用率 %.1f%% 超过阈值 %.0f%%", host.Hostname, memPct, rule.Threshold)
			}
		}
	case RuleDiskHigh:
		if host.Metrics != nil {
			for _, mt := range host.Metrics.GetMounts() {
				pct := 0.0
				if mt.GetTotal() > 0 {
					pct = float64(mt.GetUsed()) / float64(mt.GetTotal()) * 100
				}
				if pct > rule.Threshold {
					triggered = true
					message = fmt.Sprintf("主机 %s 磁盘 %s 使用率 %.1f%% 超过阈值 %.0f%%", host.Hostname, mt.GetPath(), pct, rule.Threshold)
					break
				}
			}
		}
	case RuleAnomaly:
		if m.metricsStore != nil && host.Metrics != nil {
			triggered, message = m.detectAnomaly(rule, host)
		}
	case RuleTrafficHigh:
		if host.Metrics != nil && rule.QuotaGB > 0 {
			quotaBytes := rule.QuotaGB * (1 << 30) // GiB
			used := host.Metrics.GetMonthRx() + host.Metrics.GetMonthTx()
			pct := float64(used) / quotaBytes * 100
			if pct > rule.Threshold {
				triggered = true
				message = fmt.Sprintf("主机 %s 本月流量 %s 已达配额 %s 的 %.1f%%（阈值 %.0f%%）",
					host.Hostname, fmtGiB(float64(used)/(1<<30)), fmtGiB(rule.QuotaGB), pct, rule.Threshold)
			}
		}
	}

	if triggered {
		// Rules with a Duration must hold their condition for that many seconds
		// before firing, so a single spike between two ticks cannot page anyone.
		if rule.Duration > 0 {
			held, seen := m.store.markPending(key, time.Now())
			if !seen || held < time.Duration(rule.Duration)*time.Second {
				return
			}
			message = fmt.Sprintf("%s（已持续 %s）", message, fmtDuration(held))
		}
		if !m.store.isFiring(key) {
			m.store.setFiring(key, true)
			event := &Event{
				ID:       randomID(),
				RuleID:   rule.ID,
				RuleName: rule.Name,
				Severity: rule.Severity,
				HostID:   host.ID,
				Hostname: host.Hostname,
				Message:  message,
				FiredAt:  time.Now(),
			}
			m.store.addEvent(event)
			_ = m.store.persist()
			m.log.Warn("alert fired", "rule", rule.Name, "host", host.Hostname, "severity", rule.Severity, "msg", message)
			m.notify(event)
			// Asynchronously generate AI interpretation (best-effort, non-blocking).
			m.interpretAsync(event)
		}
	} else {
		// Condition cleared — the sustain window restarts from scratch next time.
		m.store.clearPending(key)
		if m.store.isFiring(key) {
			m.store.setFiring(key, false)
			m.resolveEvent(rule.ID, host.ID)
		}
	}
}

// fmtDuration renders a sustain window in the coarsest useful unit.
func fmtDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.1f 小时", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f 分钟", d.Minutes())
	default:
		return fmt.Sprintf("%.0f 秒", d.Seconds())
	}
}

// detectAnomaly pulls the last hour of a metric from the metrics store,
// computes mean and standard deviation, and triggers when the latest sample
// deviates more than threshold × σ from the mean. threshold is interpreted as
// the σ multiplier (default 3).
func (m *Monitor) detectAnomaly(rule *Rule, host HostInfo) (bool, string) {
	sigmaMult := rule.Threshold
	if sigmaMult <= 0 {
		sigmaMult = 3
	}
	metric := rule.Metric
	if metric == "" {
		metric = "cpu"
	}

	now := time.Now().Unix()
	from := now - 3600 // last 1 hour
	pts := m.metricsStore.QueryHistory(host.ID, from, now, 15)
	if len(pts) < 10 {
		return false, "" // not enough data for statistics
	}

	var values []float64
	var latest float64
	for i, pt := range pts {
		var v float64
		switch metric {
		case "cpu":
			v = pt.CPUUsage
		case "mem":
			if pt.MemTotal > 0 {
				v = float64(pt.MemUsed) / float64(pt.MemTotal) * 100
			}
		case "net_rx":
			v = pt.NetRx
		case "net_tx":
			v = pt.NetTx
		case "disk_read":
			v = pt.DiskRead
		case "disk_write":
			v = pt.DiskWrite
		default:
			v = pt.CPUUsage
		}
		values = append(values, v)
		if i == len(pts)-1 {
			latest = v
		}
	}

	mean, std := meanStddev(values)
	if std < 0.001 {
		return false, "" // no variance, can't detect anomaly
	}

	deviation := math.Abs(latest - mean)
	if deviation > sigmaMult*std {
		return true, fmt.Sprintf("主机 %s 的 %s 指标异常：当前 %.2f 偏离均值 %.2f 达 %.1fσ（超过 %.0fσ 阈值）",
			host.Hostname, metricLabel(metric), latest, mean, deviation/std, sigmaMult)
	}
	return false, ""
}

func metricLabel(m string) string {
	switch m {
	case "cpu":
		return "CPU"
	case "mem":
		return "内存"
	case "net_rx":
		return "网络接收"
	case "net_tx":
		return "网络发送"
	case "disk_read":
		return "磁盘读"
	case "disk_write":
		return "磁盘写"
	default:
		return m
	}
}

// fmtGiB renders a GiB value with precision adapted to its magnitude.
func fmtGiB(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f GiB", v)
	case v >= 10:
		return fmt.Sprintf("%.1f GiB", v)
	default:
		return fmt.Sprintf("%.2f GiB", v)
	}
}

func meanStddev(values []float64) (mean, stddev float64) {
	n := float64(len(values))
	if n == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean = sum / n
	var sqSum float64
	for _, v := range values {
		sqSum += (v - mean) * (v - mean)
	}
	stddev = math.Sqrt(sqSum / n)
	return
}

// interpretAsync generates an AI interpretation for an alert event in the
// background and appends it to the event. Non-blocking; failures are logged.
func (m *Monitor) interpretAsync(event *Event) {
	if m.aiAssistant == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		interpretation, err := m.aiAssistant.InterpretAlert(ctx, event.Message, event.HostID, "")
		if err != nil || interpretation == "" {
			return
		}
		m.store.mu.Lock()
		for _, e := range m.store.events {
			if e.ID == event.ID {
				e.AIInterpretation = interpretation
				break
			}
		}
		m.store.mu.Unlock()
		_ = m.store.persist()
	}()
}

func (m *Monitor) resolveEvent(ruleID, hostID string) {
	m.store.mu.Lock()
	for _, e := range m.store.events {
		if e.RuleID == ruleID && e.HostID == hostID && !e.Resolved {
			e.Resolved = true
			e.ResolvedAt = time.Now()
		}
	}
	m.store.mu.Unlock()
	_ = m.store.persist()
}

func (m *Monitor) notify(e *Event) {
	cfg := m.store.GetWebhook()
	if !cfg.Enabled || cfg.URL == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		status, respStr, err := SendWebhook(ctx, cfg, e)
		if err != nil {
			m.log.Warn("webhook notify failed", "url", cfg.URL, "status", status, "err", err, "resp", respStr)
			return
		}
		m.log.Info("webhook notified successfully", "url", cfg.URL, "status", status, "resp", respStr)
	}()
}

// ---- Registry adapter ----

// registryAdapter adapts *rpc.Registry to the HostProvider interface.
type registryAdapter struct {
	reg *rpc.Registry
}

// NewHostProvider wraps a registry as a HostProvider.
func NewHostProvider(reg *rpc.Registry) HostProvider {
	return &registryAdapter{reg: reg}
}

func (a *registryAdapter) ListHosts() []HostInfo {
	agents := a.reg.ListAgents()
	out := make([]HostInfo, 0, len(agents))
	for _, ag := range agents {
		hi := HostInfo{
			ID:       ag.ID,
			Hostname: ag.Hostname,
			Group:    ag.Group,
			Status:   ag.Status,
		}
		hub := a.reg.Hub(ag.ID)
		if hub != nil {
			hi.Metrics = hub.LastMetrics()
		}
		out = append(out, hi)
	}
	return out
}