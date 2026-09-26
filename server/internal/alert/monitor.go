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
	ID              string
	Hostname        string
	Group           string
	Status          string // online / offline
	Metrics         *agentpb.MetricsSample
	ReconnectReason string // "upgrade", "maintenance"
	TrafficLimitGB  float64
	TrafficCalcType string
	TrafficResetDay int
	ExpiresAt       string // 资费到期时间（yyyy/MM/dd，来自主机财务与规格配置）
}

// HostProvider supplies the current host list to the monitor. The registry
// implements this interface.
type HostProvider interface {
	ListHosts() []HostInfo
	ClearReconnectReason(hostID string)
}

// Monitor periodically evaluates alert rules against the live host state.
type Monitor struct {
	store        *Store
	provider     HostProvider
	metricsStore *metrics.Store
	aiAssistant  *ai.Assistant
	log          *slog.Logger
	stop         chan struct{}
	stopOnce     sync.Once

	mu        sync.Mutex
	startTime time.Time
	bootGrace time.Duration
	ready     bool

	prevStatus           map[string]string    // hostID -> "online" | "offline"
	startupOnlinePending map[string]bool      // known-at-start offline hosts: suppress first reconnect
	maintenanceUntil     map[string]time.Time // hostID -> maintenance expiration time
}

// NewMonitor creates a monitor that evaluates rules every interval.
// metricsStore and aiAssistant are optional (enable anomaly detection + AI
// interpretation when non-nil).
func NewMonitor(store *Store, provider HostProvider, metricsStore *metrics.Store, aiAssistant *ai.Assistant, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	return &Monitor{
		store:                store,
		provider:             provider,
		metricsStore:         metricsStore,
		aiAssistant:          aiAssistant,
		log:                  log,
		stop:                 make(chan struct{}),
		startTime:            time.Now(),
		bootGrace:            90 * time.Second, // aligns with reaper window (heartbeatSec 30s * heartbeatGraceFactor 3)
		prevStatus:           make(map[string]string),
		startupOnlinePending: make(map[string]bool),
		maintenanceUntil:     make(map[string]time.Time),
	}
}

// SetHostMaintenance sets a maintenance suppression window for a host (e.g. during upgrade).
// While active, online and offline state changes will not trigger alert events.
func (m *Monitor) SetHostMaintenance(hostID string, duration time.Duration, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	until := time.Now().Add(duration)
	m.maintenanceUntil[hostID] = until
	m.log.Info("host placed into scheduled maintenance", "host_id", hostID, "duration", duration, "reason", reason, "until", until)
}

// InMaintenance checks if a host is currently within an active maintenance window.
func (m *Monitor) InMaintenance(hostID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.maintenanceUntil[hostID]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(m.maintenanceUntil, hostID)
		return false
	}
	return true
}

// ClearHostMaintenance ends a maintenance window early (e.g. the host is
// unbound or the upgrade was cancelled).
func (m *Monitor) ClearHostMaintenance(hostID string) {
	m.mu.Lock()
	delete(m.maintenanceUntil, hostID)
	m.mu.Unlock()
}

// sweepMaintenance drops expired maintenance windows. InMaintenance only
// cleans up the entries it happens to be asked about, so without this sweep
// hosts that go away (unbound, never evaluated again) leak map entries.
func (m *Monitor) sweepMaintenance() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, until := range m.maintenanceUntil {
		if now.After(until) {
			delete(m.maintenanceUntil, id)
		}
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
	m.stopOnce.Do(func() {
		close(m.stop)
	})
}

func (m *Monitor) evaluate() {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("alert monitor evaluate recovered from panic", "panic", r)
		}
	}()

	m.log.Debug("alert monitor evaluate tick started")

	// Alert storm suppression: if >20 events fired in the last 5 minutes,
	// collapse them into a single summary event and downgrade the rest.
	m.suppressStorm()
	m.sweepMaintenance()

	rules := m.store.ListRules()
	hosts := m.provider.ListHosts()

	m.mu.Lock()
	firstRun := !m.ready && len(m.prevStatus) == 0
	isBootstrapping := !m.ready && time.Since(m.startTime) < m.bootGrace
	boundaryRun := false
	if firstRun {
		// Establish a baseline from the persisted registry. Hosts may be loaded
		// as offline and reconnect asynchronously; none of this is an alert.
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
	}
	if !isBootstrapping && !m.ready {
		// The first tick after the grace window is a synchronization boundary,
		// not a notification tick. This catches agents that reconnect just after
		// the grace deadline and prevents a false offline -> online transition.
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
		m.ready = true
		boundaryRun = true
	}
	m.mu.Unlock()

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		// During startup and on the first post-startup synchronization tick,
		// suppress both edge rules. Resource rules continue to be evaluated.
		if (isBootstrapping || boundaryRun) && (rule.Type == RuleOnline || rule.Type == RuleOffline) {
			continue
		}
		for _, host := range hosts {
			if !matchesFilter(host, rule) {
				continue
			}
			m.checkRule(rule, host)
		}
	}

	// 评估各主机的专属月流量配额超标（达到80%警告，达到100%严重）
	for _, host := range hosts {
		m.checkHostTraffic(host)
		// 到期时间前 30 天发起一次续费提醒
		m.checkHostExpiry(host)
	}

	m.mu.Lock()
	for _, host := range hosts {
		m.prevStatus[host.ID] = host.Status
	}
	m.mu.Unlock()

	m.log.Debug("alert monitor evaluate tick finished", "rules", len(rules), "hosts", len(hosts))
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

// checkHostTraffic directly evaluates host-specific traffic quotas (80% warning, 100% critical)
// without requiring manual setup in the global alert rule list.
func (m *Monitor) checkHostTraffic(host HostInfo) {
	quotaGB := host.TrafficLimitGB
	if quotaGB <= 0 || host.Metrics == nil {
		return
	}

	var used int64
	calcDesc := "双向"
	switch host.TrafficCalcType {
	case "out":
		used = host.Metrics.GetMonthTx()
		calcDesc = "出向"
	case "in":
		used = host.Metrics.GetMonthRx()
		calcDesc = "入向"
	default:
		used = host.Metrics.GetMonthRx() + host.Metrics.GetMonthTx()
		calcDesc = "双向"
	}

	quotaBytes := quotaGB * (1 << 30) // GiB
	pct := float64(used) / quotaBytes * 100

	// We define two tiers: 100% (critical) and 80% (warning)
	checkTier := func(tierName string, threshold float64, sev Severity) {
		key := "builtin-traffic-" + tierName + ":" + host.ID
		if pct >= threshold {
			if !m.store.isFiring(key) {
				m.store.setFiring(key, true)
				msg := fmt.Sprintf("主机 %s 本月流量(%s) %s 已达到配额 %s 的 %.1f%% (告警阈值 %.0f%%)",
					host.Hostname, calcDesc, fmtGiB(float64(used)/(1<<30)), fmtGiB(quotaGB), pct, threshold)
				event := &Event{
					ID:       randomID(),
					RuleID:   "builtin-traffic-" + tierName,
					RuleName: "月流量超额预警 (" + tierName + ")",
					Severity: sev,
					HostID:   host.ID,
					Hostname: host.Hostname,
					Message:  msg,
					FiredAt:  time.Now(),
				}
				m.store.addEvent(event)
				_ = m.store.persist()
				m.log.Warn("traffic quota alert fired", "host", host.Hostname, "severity", sev, "msg", msg)
				m.notify(event)
				m.interpretAsync(event)
			}
		} else {
			if m.store.isFiring(key) {
				m.store.setFiring(key, false)
				m.resolveEvent("builtin-traffic-"+tierName, host.ID)
			}
		}
	}

	checkTier("80", 80, SeverityWarning)
	checkTier("100", 100, SeverityCritical)
}

// checkHostExpiry 在主机配置的到期时间前 30 天发起一次续费提醒（告警中心 + webhook），
// 只触发一次；续费使到期时间重新推后到 30 天以外后自动解除，逾期未续则保持提醒不重复打扰。
func (m *Monitor) checkHostExpiry(host HostInfo) {
	if host.ExpiresAt == "" {
		return
	}
	expiry, err := parseBillingDate(host.ExpiresAt)
	if err != nil {
		return
	}
	daysLeft := int(math.Ceil(time.Until(expiry).Hours() / 24))
	key := "builtin-expiry-remind:" + host.ID
	if daysLeft <= 30 {
		if !m.store.isFiring(key) {
			m.store.setFiring(key, true)
			var msg string
			switch {
			case daysLeft < 0:
				msg = fmt.Sprintf("主机 %s 资费已于 %s 到期，请尽快续费", host.Hostname, host.ExpiresAt)
			case daysLeft == 0:
				msg = fmt.Sprintf("主机 %s 资费将于今天 (%s) 到期，请及时续费", host.Hostname, host.ExpiresAt)
			default:
				msg = fmt.Sprintf("主机 %s 资费将于 %s 到期（剩余 %d 天），请提前安排续费", host.Hostname, host.ExpiresAt, daysLeft)
			}
			event := &Event{
				ID:       randomID(),
				RuleID:   "builtin-expiry-remind",
				RuleName: "主机到期续费提醒",
				Severity: SeverityWarning,
				HostID:   host.ID,
				Hostname: host.Hostname,
				Message:  msg,
				FiredAt:  time.Now(),
			}
			m.store.addEvent(event)
			_ = m.store.persist()
			m.log.Info("host expiry reminder fired", "host", host.Hostname, "expires_at", host.ExpiresAt, "days_left", daysLeft)
			m.notify(event)
		}
	} else if m.store.isFiring(key) {
		// 已续费（到期时间推后到 30 天以外），解除提醒
		m.store.setFiring(key, false)
		m.resolveEvent("builtin-expiry-remind", host.ID)
	}
}

// parseBillingDate 解析「财务与规格」中配置的到期日期（yyyy/MM/dd，兼容 yyyy-MM-dd）。
func parseBillingDate(s string) (time.Time, error) {
	for _, layout := range []string{"2006/01/02", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date format: %s", s)
}

func (m *Monitor) checkRule(rule *Rule, host HostInfo) {
	key := rule.ID + ":" + host.ID
	triggered := false
	message := ""

	switch rule.Type {
	case RuleOffline:
		if host.Status != "online" {
			// Suppress offline alert if the host is in an active maintenance window.
			if m.InMaintenance(host.ID) {
				// Reset the sustain window: without this a Duration rule keeps
				// banking time while suppressed and fires immediately once
				// maintenance ends, even for a fresh outage.
				m.store.clearPending(key)
				m.log.Debug("suppressing offline alert during maintenance", "host_id", host.ID, "hostname", host.Hostname)
				return
			}
			triggered = true
			message = fmt.Sprintf("主机 %s 已离线", host.Hostname)
		}
	case RuleOnline:
		m.mu.Lock()
		prev, hasPrev := m.prevStatus[host.ID]
		startupPending := m.startupOnlinePending[host.ID]
		booting := !m.ready
		m.mu.Unlock()
		if host.Status == "online" {
			// If host reconnected after upgrade or maintenance, clear maintenance and suppress online alert
			if host.ReconnectReason == "upgrade" || host.ReconnectReason == "maintenance" {
				m.mu.Lock()
				delete(m.maintenanceUntil, host.ID)
				m.mu.Unlock()
				m.store.setFiring(key, true)
				m.log.Info("host reconnected with planned reason, suppressing online alert", "host_id", host.ID, "hostname", host.Hostname, "reason", host.ReconnectReason)
				m.provider.ClearReconnectReason(host.ID)
				return
			}
			if m.InMaintenance(host.ID) {
				m.store.setFiring(key, true)
				m.log.Debug("suppressing online alert during maintenance", "host_id", host.ID, "hostname", host.Hostname)
				return
			}
			if booting && startupPending {
				// The host was already known before this server boot and has
				// merely reconnected during the grace window; never notify.
				return
			}

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
			ID:              ag.ID,
			Hostname:        ag.Hostname,
			Group:           ag.Group,
			Status:          ag.Status,
			ReconnectReason: ag.ReconnectReason,
			TrafficLimitGB:  ag.TrafficLimitGB,
			TrafficCalcType: ag.TrafficCalcType,
			TrafficResetDay: ag.TrafficResetDay,
			ExpiresAt:       ag.ExpiresAt,
		}
		hub := a.reg.Hub(ag.ID)
		if hub != nil {
			hi.Metrics = hub.LastMetrics()
		}
		out = append(out, hi)
	}
	return out
}

func (a *registryAdapter) ClearReconnectReason(hostID string) {
	if a.reg != nil {
		a.reg.ClearReconnectReason(hostID)
	}
}
