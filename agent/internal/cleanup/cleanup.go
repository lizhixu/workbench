// Package cleanup implements per-host system cleanup on the agent: preview
// (reclaimable bytes per category, served through the sysinfo "cleanup"
// kind) and execution (a typed CleanupRequest, one fixed argv per category,
// before/after measurement for the reclaimed amount).
//
// Categories are fixed and validated on both ends; no user-supplied path
// ever reaches a command line. Only Debian-family hosts are supported
// (apt + journalctl); other systems report unsupported instead of running.
package cleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"watchman/proto/agentpb"
)

// Sender pushes agent->server messages.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// Runner executes one command and returns its stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func defaultRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func readOSReleaseFile() (string, error) {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Category is one cleanable class.
type Category struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Desc  string `json:"desc"`
}

// Categories lists the v1 categories in display order.
func Categories() []Category {
	return []Category{
		{Name: "apt_cache", Title: "APT 缓存", Desc: "apt-get clean，清理 /var/cache/apt/archives 下的安装包缓存"},
		{Name: "journal", Title: "journal 日志", Desc: "journalctl vacuum，保留最近 7 天且总量不超过 500MB"},
		{Name: "tmp_stale", Title: "临时文件", Desc: "/tmp 与 /var/tmp 中 10 天未访问的普通文件"},
		{Name: "logs_old", Title: "旧日志", Desc: "/var/log 中 30 天以上的压缩日志（.gz）"},
	}
}

func validCategory(name string) bool {
	for _, c := range Categories() {
		if c.Name == name {
			return true
		}
	}
	return false
}

// Journal limits: vacuum keeps at most 7 days and 500MiB.
const (
	journalKeepSize = 500 * 1024 * 1024
)

// isDebianLike reports whether an /etc/os-release body belongs to the
// Debian family (Debian itself, Ubuntu, or anything declaring ID_LIKE=debian).
func isDebianLike(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		v := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		switch strings.TrimSpace(kv[0]) {
		case "ID":
			if v == "debian" || v == "ubuntu" {
				return true
			}
		case "ID_LIKE":
			for _, f := range strings.Fields(v) {
				if f == "debian" {
					return true
				}
			}
		}
	}
	return false
}

// osID extracts the ID= value for display ("debian", "ubuntu", ...).
func osID(body string) string {
	for _, line := range strings.Split(body, "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.TrimSpace(kv[0]) == "ID" {
			return strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		}
	}
	return ""
}

var sizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGT]i?B?)?$`)

// parseSize parses "123", "1.5K", "500M", "1.2G" with 1024-based units,
// matching du/journalctl output.
func parseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("无法解析大小 %q", s)
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := float64(1)
	u := strings.ToUpper(m[2])
	u = strings.TrimSuffix(u, "B")
	u = strings.TrimSuffix(u, "I")
	switch u {
	case "":
	case "K":
		mult = 1024
	case "M":
		mult = 1024 * 1024
	case "G":
		mult = 1024 * 1024 * 1024
	case "T":
		mult = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("无法解析大小 %q", s)
	}
	return int64(f * mult), nil
}

var journalUsageRe = regexp.MustCompile(`take up ([0-9.]+[KMGT]i?B?)`)

// parseJournalUsage parses `journalctl --disk-usage` output
// ("Archived and active journals take up 1.2G in the file system.").
func parseJournalUsage(out string) (int64, error) {
	m := journalUsageRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("无法解析 journal 用量：%q", strings.TrimSpace(out))
	}
	return parseSize(m[1])
}

// tmpFindArgs builds the find argv shared by tmp_stale measurement and
// deletion: regular files under /tmp and /var/tmp not accessed in 10 days,
// excluding systemd private directories (live service state, never touched).
func tmpFindArgs(extra ...string) []string {
	base := []string{"find", "/tmp", "/var/tmp", "-type", "f", "-atime", "+10",
		"-not", "-path", "/tmp/systemd-private-*",
		"-not", "-path", "/tmp/systemd-private-*/*",
		"-not", "-path", "/var/tmp/systemd-private-*",
		"-not", "-path", "/var/tmp/systemd-private-*/*",
	}
	return append(base, extra...)
}

func logsFindArgs(extra ...string) []string {
	base := []string{"find", "/var/log", "-type", "f", "-name", "*.gz", "-mtime", "+30"}
	return append(base, extra...)
}

// sumFindSizes runs find -printf '%s\n' and sums the file sizes.
func sumFindSizes(ctx context.Context, run Runner, argv []string) (int64, error) {
	out, err := run(ctx, argv[0], argv[1:]...)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("find 输出无法解析：%q", line)
		}
		total += n
	}
	return total, nil
}

// measureCurrent returns the current on-disk size backing one category.
func measureCurrent(ctx context.Context, run Runner, name string) (int64, error) {
	switch name {
	case "apt_cache":
		out, err := run(ctx, "du", "-sb", "/var/cache/apt/archives")
		if err != nil {
			return 0, fmt.Errorf("du apt 缓存失败: %w", err)
		}
		f := strings.Fields(string(out))
		if len(f) == 0 {
			return 0, fmt.Errorf("du 输出为空")
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("du 输出无法解析：%q", f[0])
		}
		return n, nil
	case "journal":
		out, err := run(ctx, "journalctl", "--disk-usage")
		if err != nil {
			return 0, fmt.Errorf("journalctl 失败: %w", err)
		}
		return parseJournalUsage(string(out))
	case "tmp_stale":
		return sumFindSizes(ctx, run, tmpFindArgs("-printf", "%s\n"))
	case "logs_old":
		return sumFindSizes(ctx, run, logsFindArgs("-printf", "%s\n"))
	}
	return 0, fmt.Errorf("未知清理类别: %s", name)
}

// measureReclaimable returns the bytes a preview may promise for one
// category. Only journal differs from current size: vacuum applies both
// the 7-day and the 500MB cap, so the preview is a lower bound — the real
// run can free more, never less, than this number.
func measureReclaimable(ctx context.Context, run Runner, name string) (int64, error) {
	cur, err := measureCurrent(ctx, run, name)
	if err != nil {
		return 0, err
	}
	if name == "journal" && cur > journalKeepSize {
		return cur - journalKeepSize, nil
	}
	if name == "journal" {
		return 0, nil
	}
	return cur, nil
}

// executeArgv returns the fixed argv for one category: [binary, args...].
// Nothing here takes user input; categories are validated before use.
func executeArgv(name string) ([]string, bool) {
	switch name {
	case "apt_cache":
		return []string{"apt-get", "clean"}, true
	case "journal":
		return []string{"journalctl", "--vacuum-size=500M", "--vacuum-time=7d"}, true
	case "tmp_stale":
		return tmpFindArgs("-delete"), true
	case "logs_old":
		return logsFindArgs("-delete"), true
	}
	return nil, false
}

// stepResult is one executed category in the CleanupEvent payload.
type stepResult struct {
	Name      string `json:"name"`
	Reclaimed int64  `json:"reclaimed"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// Manager handles CleanupRequests.
type Manager struct {
	mu     sync.Mutex
	sender Sender
	log    *slog.Logger

	// run and readOS are swappable in tests.
	run    Runner
	readOS func() (string, error)
}

func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, run: defaultRunner, readOS: readOSReleaseFile}
}

func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	m.sender = s
	m.mu.Unlock()
}

func (m *Manager) send(ev *agentpb.CleanupEvent) {
	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s != nil {
		s.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_Cleanup{Cleanup: ev},
		})
	}
}

// Handle processes a CleanupRequest from the server.
func (m *Manager) Handle(req *agentpb.CleanupRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ev := &agentpb.CleanupEvent{CleanupId: req.GetCleanupId()}
	steps, ok, err := m.execute(ctx, req.GetCategories())
	if !ok {
		ev.Ok = false
		ev.Error = err.Error()
		m.send(ev)
		return
	}
	payload, err := json.Marshal(map[string]any{"steps": steps})
	if err != nil {
		ev.Ok = false
		ev.Error = err.Error()
		m.send(ev)
		return
	}
	ev.Ok = true
	ev.PayloadJson = payload
	m.send(ev)
}

// execute validates the request and runs each category in order. A failed
// step does not stop the remaining steps; failures are reported per step.
func (m *Manager) execute(ctx context.Context, categories []string) ([]stepResult, bool, error) {
	if len(categories) == 0 {
		return nil, false, fmt.Errorf("未选择清理类别")
	}
	seen := map[string]bool{}
	for _, c := range categories {
		if !validCategory(c) {
			return nil, false, fmt.Errorf("未知清理类别: %s", c)
		}
		if seen[c] {
			return nil, false, fmt.Errorf("重复的清理类别: %s", c)
		}
		seen[c] = true
	}
	body, err := m.readOS()
	if err != nil {
		return nil, false, fmt.Errorf("读取系统版本失败: %w", err)
	}
	if !isDebianLike(body) {
		return nil, false, fmt.Errorf("当前系统（%s）不支持系统清理，仅支持 Debian/Ubuntu", osID(body))
	}
	steps := make([]stepResult, 0, len(categories))
	for _, name := range categories {
		steps = append(steps, m.runStep(ctx, name))
	}
	return steps, true, nil
}

func (m *Manager) runStep(ctx context.Context, name string) stepResult {
	before, err := measureCurrent(ctx, m.run, name)
	if err != nil {
		return stepResult{Name: name, OK: false, Error: err.Error()}
	}
	argv, ok := executeArgv(name)
	if !ok {
		return stepResult{Name: name, OK: false, Error: "未知清理类别: " + name}
	}
	if _, err := m.run(ctx, argv[0], argv[1:]...); err != nil {
		return stepResult{Name: name, OK: false, Error: err.Error()}
	}
	after, err := measureCurrent(ctx, m.run, name)
	if err != nil {
		// 清理已执行，但复核失败：回收量未知，不谎报数字。
		return stepResult{Name: name, OK: false, Error: "清理已执行，复核用量失败: " + err.Error()}
	}
	reclaimed := before - after
	if reclaimed < 0 {
		reclaimed = 0
	}
	return stepResult{Name: name, Reclaimed: reclaimed, OK: true}
}

// previewCategory is one row of the preview payload.
type previewCategory struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Desc        string `json:"desc"`
	Reclaimable int64  `json:"reclaimable"`
	Error       string `json:"error,omitempty"`
}

type previewResult struct {
	OS         string            `json:"os"`
	Supported  bool              `json:"supported"`
	Categories []previewCategory `json:"categories"`
}

// Preview computes the reclaimable bytes per category. An unsupported OS
// returns supported:false instead of an error so the UI can say so plainly;
// a per-category measurement failure is reported on that row, not fatal.
func Preview(ctx context.Context, run Runner, readOS func() (string, error)) ([]byte, error) {
	body, err := readOS()
	osName := ""
	supported := false
	if err == nil {
		osName = osID(body)
		supported = isDebianLike(body)
	}
	res := previewResult{OS: osName, Supported: supported}
	for _, c := range Categories() {
		pc := previewCategory{Name: c.Name, Title: c.Title, Desc: c.Desc}
		if supported {
			n, err := measureReclaimable(ctx, run, c.Name)
			if err != nil {
				pc.Error = err.Error()
			} else {
				pc.Reclaimable = n
			}
		}
		res.Categories = append(res.Categories, pc)
	}
	return json.Marshal(res)
}

// PreviewDefault runs Preview with the live system commands (used by the
// sysinfo "cleanup" kind).
func PreviewDefault(ctx context.Context) ([]byte, error) {
	return Preview(ctx, defaultRunner, readOSReleaseFile)
}
