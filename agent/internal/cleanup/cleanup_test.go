package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"watchman/proto/agentpb"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"123", 123, true},
		{"1.5K", 1536, true},
		{"2M", 2 * 1024 * 1024, true},
		{"500M", 500 * 1024 * 1024, true},
		{"1.2G", 1288490188, true},
		{"1T", 1024 * 1024 * 1024 * 1024, true},
		{"1.2GB", 1288490188, true},
		{"abc", 0, false},
		{"", 0, false},
		{"12X", 0, false},
	}
	for _, tc := range cases {
		got, err := parseSize(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("parseSize(%q) = %d, %v, 想要 %d", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseSize(%q) 应失败，实际 %d", tc.in, got)
		}
	}
}

func TestParseJournalUsage(t *testing.T) {
	got, err := parseJournalUsage("Archived and active journals take up 1.2G in the file system.\n")
	if err != nil || got != 1288490188 {
		t.Errorf("parseJournalUsage = %d, %v, 想要 1288490188", got, err)
	}
	got, err = parseJournalUsage("Archived and active journals take up 456M in the file system.")
	if err != nil || got != 456*1024*1024 {
		t.Errorf("parseJournalUsage = %d, %v, 想要 %d", got, err, 456*1024*1024)
	}
	if _, err := parseJournalUsage("no journals here"); err == nil {
		t.Error("parseJournalUsage 对无用量输出应失败")
	}
}

func TestIsDebianLike(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"ID=debian\nVERSION_ID=\"12\"\n", true},
		{"ID=ubuntu\nID_LIKE=debian\n", true},
		{"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", true},
		{"ID=arch\n", false},
		{"ID=centos\nID_LIKE=\"rhel fedora\"\n", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isDebianLike(tc.body); got != tc.want {
			t.Errorf("isDebianLike(%q) = %v, 想要 %v", tc.body, got, tc.want)
		}
	}
	if got := osID("ID=ubuntu\nID_LIKE=debian\n"); got != "ubuntu" {
		t.Errorf("osID = %q, 想要 ubuntu", got)
	}
}

func TestExecuteArgv(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"apt_cache", "apt-get clean"},
		{"journal", "journalctl --vacuum-size=500M --vacuum-time=7d"},
		{"logs_old", "find /var/log -type f -name *.gz -mtime +30 -delete"},
	}
	for _, tc := range cases {
		argv, ok := executeArgv(tc.name)
		if !ok {
			t.Errorf("executeArgv(%q) 应被接受", tc.name)
			continue
		}
		if strings.Join(argv, " ") != tc.want {
			t.Errorf("executeArgv(%q) = %q, 想要 %q", tc.name, strings.Join(argv, " "), tc.want)
		}
	}
	// tmp_stale 的 argv 含 systemd 私有目录排除，单独断言关键片段。
	argv, ok := executeArgv("tmp_stale")
	if !ok {
		t.Fatal("executeArgv(tmp_stale) 应被接受")
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"find /tmp /var/tmp -type f -atime +10",
		"-not -path /tmp/systemd-private-*",
		"-not -path /var/tmp/systemd-private-*",
		"-delete",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("executeArgv(tmp_stale) = %q, 缺少 %q", joined, want)
		}
	}
	if _, ok := executeArgv("autoremove"); ok {
		t.Error("executeArgv(autoremove) 不应在 v1 类别里")
	}
}

// fakeRunner 按命令分发 canned 输出；队列式，同一命令多次调用依次出队。
type fakeRunner struct {
	calls []string
	seq   map[string][][]byte
	fail  map[string]error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, key)
	if err, ok := f.fail[key]; ok {
		return nil, err
	}
	q := f.seq[key]
	if len(q) == 0 {
		return []byte{}, nil
	}
	out := q[0]
	f.seq[key] = q[1:]
	return out, nil
}

func TestMeasureReclaimable(t *testing.T) {
	fr := &fakeRunner{seq: map[string][][]byte{
		"du -sb /var/cache/apt/archives": {[]byte("12345\t/var/cache/apt/archives\n")},
		"journalctl --disk-usage": {
			[]byte("Archived and active journals take up 1.2G in the file system.\n"),
			[]byte("Archived and active journals take up 100M in the file system.\n"),
		},
	}}
	ctx := context.Background()
	if got, err := measureReclaimable(ctx, fr.run, "apt_cache"); err != nil || got != 12345 {
		t.Errorf("apt_cache = %d, %v, 想要 12345", got, err)
	}
	// journal 预览是下界：1.2G - 500M。
	if got, err := measureReclaimable(ctx, fr.run, "journal"); err != nil || got != 1288490188-500*1024*1024 {
		t.Errorf("journal = %d, %v, 想要 %d", got, err, 1288490188-500*1024*1024)
	}
	// 100M 低于 500M 上限，可回收为 0。
	if got, err := measureReclaimable(ctx, fr.run, "journal"); err != nil || got != 0 {
		t.Errorf("journal 小用量 = %d, %v, 想要 0", got, err)
	}
	if _, err := measureReclaimable(ctx, fr.run, "nope"); err == nil {
		t.Error("未知类别应失败")
	}
}

func TestMeasureTmpAndLogs(t *testing.T) {
	fr := &fakeRunner{seq: map[string][][]byte{}}
	ctx := context.Background()
	// find 的 -printf 输出按行求和；空输出为 0。
	fr.seq[strings.Join(tmpFindArgs("-printf", "%s\n"), " ")] = [][]byte{[]byte("100\n200\n")}
	if got, err := measureCurrent(ctx, fr.run, "tmp_stale"); err != nil || got != 300 {
		t.Errorf("tmp_stale = %d, %v, 想要 300", got, err)
	}
	fr.seq[strings.Join(logsFindArgs("-printf", "%s\n"), " ")] = [][]byte{[]byte("")}
	if got, err := measureCurrent(ctx, fr.run, "logs_old"); err != nil || got != 0 {
		t.Errorf("logs_old 空 = %d, %v, 想要 0", got, err)
	}
}

type fakeSender struct {
	msgs []*agentpb.AgentMessage
}

func (f *fakeSender) Send(msg *agentpb.AgentMessage) bool {
	f.msgs = append(f.msgs, msg)
	return true
}

func newTestManager(fr *fakeRunner, osRelease string) (*Manager, *fakeSender) {
	s := &fakeSender{}
	m := &Manager{
		log: slog.Default(),
		run: fr.run,
		readOS: func() (string, error) {
			return osRelease, nil
		},
	}
	m.SetSender(s)
	return m, s
}

func lastCleanupEvent(t *testing.T, s *fakeSender) *agentpb.CleanupEvent {
	t.Helper()
	if len(s.msgs) != 1 {
		t.Fatalf("应恰好发送 1 条消息，实际 %d", len(s.msgs))
	}
	ev := s.msgs[0].GetCleanup()
	if ev == nil {
		t.Fatal("消息不是 CleanupEvent")
	}
	return ev
}

func TestManagerHandleHappyPath(t *testing.T) {
	fr := &fakeRunner{seq: map[string][][]byte{
		"du -sb /var/cache/apt/archives": {[]byte("1000\t/var/cache/apt/archives\n"), []byte("200\t/var/cache/apt/archives\n")},
		"journalctl --disk-usage": {
			[]byte("Archived and active journals take up 600M in the file system.\n"),
			[]byte("Archived and active journals take up 500M in the file system.\n"),
		},
	}}
	tmpKey := strings.Join(tmpFindArgs("-printf", "%s\n"), " ")
	logsKey := strings.Join(logsFindArgs("-printf", "%s\n"), " ")
	fr.seq[tmpKey] = [][]byte{[]byte("300\n"), []byte("0\n")}
	fr.seq[logsKey] = [][]byte{[]byte("50\n"), []byte("50\n")}

	m, s := newTestManager(fr, "ID=debian\nVERSION_ID=\"12\"\n")
	m.Handle(&agentpb.CleanupRequest{CleanupId: "c1", Categories: []string{"apt_cache", "journal", "tmp_stale", "logs_old"}})
	ev := lastCleanupEvent(t, s)
	if !ev.GetOk() {
		t.Fatalf("Handle 应成功，实际 error=%q", ev.GetError())
	}
	var payload struct {
		Steps []stepResult `json:"steps"`
	}
	if err := json.Unmarshal(ev.GetPayloadJson(), &payload); err != nil {
		t.Fatalf("payload 解析失败：%v", err)
	}
	if len(payload.Steps) != 4 {
		t.Fatalf("应有 4 步，实际 %d", len(payload.Steps))
	}
	want := []struct {
		name      string
		reclaimed int64
	}{
		{"apt_cache", 800},
		{"journal", 100 * 1024 * 1024},
		{"tmp_stale", 300},
		{"logs_old", 0},
	}
	for i, w := range want {
		st := payload.Steps[i]
		if st.Name != w.name || st.Reclaimed != w.reclaimed || !st.OK {
			t.Errorf("步骤 %d = %+v, 想要 name=%s reclaimed=%d ok=true", i, st, w.name, w.reclaimed)
		}
	}
	// 执行命令确实按类别下发（固定 argv，无用户输入）。
	joined := strings.Join(fr.calls, "\n")
	for _, wantCall := range []string{
		"apt-get clean",
		"journalctl --vacuum-size=500M --vacuum-time=7d",
		"-delete",
	} {
		if !strings.Contains(joined, wantCall) {
			t.Errorf("未下发 %q，调用记录：\n%s", wantCall, joined)
		}
	}
}

func TestManagerHandleRejects(t *testing.T) {
	fr := &fakeRunner{}
	m, s := newTestManager(fr, "ID=debian\n")
	m.Handle(&agentpb.CleanupRequest{CleanupId: "c1", Categories: []string{}})
	if ev := lastCleanupEvent(t, s); ev.GetOk() || ev.GetError() == "" {
		t.Errorf("空类别应拒绝，实际 ok=%v error=%q", ev.GetOk(), ev.GetError())
	}
	s.msgs = nil
	m.Handle(&agentpb.CleanupRequest{CleanupId: "c2", Categories: []string{"autoremove"}})
	if ev := lastCleanupEvent(t, s); ev.GetOk() {
		t.Error("未知类别应拒绝")
	}
	s.msgs = nil
	m.Handle(&agentpb.CleanupRequest{CleanupId: "c3", Categories: []string{"apt_cache", "apt_cache"}})
	if ev := lastCleanupEvent(t, s); ev.GetOk() {
		t.Error("重复类别应拒绝")
	}
	// 非 Debian 系整个请求拒绝，不执行任何命令。
	s.msgs = nil
	mArch, sArch := newTestManager(fr, "ID=arch\n")
	mArch.Handle(&agentpb.CleanupRequest{CleanupId: "c4", Categories: []string{"apt_cache"}})
	if ev := lastCleanupEvent(t, sArch); ev.GetOk() || !strings.Contains(ev.GetError(), "不支持") {
		t.Errorf("Arch 应拒绝，实际 ok=%v error=%q", ev.GetOk(), ev.GetError())
	}
	if len(fr.calls) != 0 {
		t.Errorf("拒绝后不应执行命令，实际调用：%v", fr.calls)
	}
}

func TestManagerHandleStepFailureDoesNotStopOthers(t *testing.T) {
	fr := &fakeRunner{
		seq: map[string][][]byte{
			"du -sb /var/cache/apt/archives": {[]byte("1000\t/x\n"), []byte("200\t/x\n")},
			"journalctl --disk-usage":        {[]byte("Archived and active journals take up 600M in the file system.\n")},
		},
		fail: map[string]error{
			"journalctl --vacuum-size=500M --vacuum-time=7d": errors.New("exit status 1"),
		},
	}
	m, s := newTestManager(fr, "ID=ubuntu\n")
	m.Handle(&agentpb.CleanupRequest{CleanupId: "c1", Categories: []string{"apt_cache", "journal"}})
	ev := lastCleanupEvent(t, s)
	if !ev.GetOk() {
		t.Fatalf("单步失败不应让整个请求失败，实际 error=%q", ev.GetError())
	}
	var payload struct {
		Steps []stepResult `json:"steps"`
	}
	if err := json.Unmarshal(ev.GetPayloadJson(), &payload); err != nil {
		t.Fatalf("payload 解析失败：%v", err)
	}
	if len(payload.Steps) != 2 || !payload.Steps[0].OK || payload.Steps[1].OK {
		t.Errorf("步骤结果不符合预期：%+v", payload.Steps)
	}
	if payload.Steps[1].Error == "" {
		t.Error("失败步骤应带 error")
	}
}

func TestPreview(t *testing.T) {
	fr := &fakeRunner{seq: map[string][][]byte{
		"du -sb /var/cache/apt/archives": {[]byte("12345\t/x\n")},
		"journalctl --disk-usage":        {[]byte("Archived and active journals take up 100M in the file system.\n")},
	}}
	tmpKey := strings.Join(tmpFindArgs("-printf", "%s\n"), " ")
	logsKey := strings.Join(logsFindArgs("-printf", "%s\n"), " ")
	fr.seq[tmpKey] = [][]byte{[]byte("300\n")}
	fr.seq[logsKey] = [][]byte{[]byte("")}
	ctx := context.Background()
	payload, err := Preview(ctx, fr.run, func() (string, error) { return "ID=debian\n", nil })
	if err != nil {
		t.Fatalf("Preview 失败：%v", err)
	}
	var res previewResult
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatalf("Preview 解析失败：%v", err)
	}
	if !res.Supported || res.OS != "debian" || len(res.Categories) != 4 {
		t.Errorf("Preview 结构不符合预期：%+v", res)
	}
	if res.Categories[0].Reclaimable != 12345 || res.Categories[2].Reclaimable != 300 {
		t.Errorf("Preview 数值不符合预期：%+v", res.Categories)
	}
	// 非 Debian 系：supported=false，不报错，前端据此显示不支持。
	payload, err = Preview(ctx, fr.run, func() (string, error) { return "ID=arch\n", nil })
	if err != nil {
		t.Fatalf("Preview 非 Debian 不应报错：%v", err)
	}
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatalf("Preview 解析失败：%v", err)
	}
	if res.Supported || res.OS != "arch" {
		t.Errorf("Arch Preview 应为 supported=false，实际 %+v", res)
	}
}
