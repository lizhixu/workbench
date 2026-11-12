package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Start/End 的基本 round-trip：开始时 live、时长 0；结束后落终态。
func TestStartEndRoundTrip(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.Start("s1", "agent-1", "host-1", "alice")

	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("Start 后应能取到记录")
	}
	if !got.Live || got.Duration != 0 || !got.EndedAt.IsZero() {
		t.Errorf("开始时应为 live/0s/无结束时间，实际 live=%v duration=%d ended=%v", got.Live, got.Duration, got.EndedAt)
	}

	s.End("s1")
	got, _ = s.Get("s1")
	if got.Live {
		t.Error("End 后 Live 应为 false")
	}
	if got.EndedAt.IsZero() || got.EndedAt.Before(got.StartedAt) {
		t.Errorf("End 后应有合法结束时间，实际 ended=%v started=%v", got.EndedAt, got.StartedAt)
	}
	if got.Duration < 0 {
		t.Errorf("时长不应为负，实际 %d", got.Duration)
	}
}

// End 必须幂等：PTY 退出时调一次、宽限期到期清理时再调一次，第二次不能把
// EndedAt 往后推、把时长撑大。这里直接把 StartedAt 往前拨来模拟时间流逝，
// 不靠 sleep 等墙钟。
func TestEndIdempotent(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.Start("s1", "agent-1", "host-1", "alice")

	// 让会话“已经跑了 5 秒”。
	s.mu.Lock()
	s.sessions["s1"].StartedAt = time.Now().Add(-5 * time.Second)
	s.mu.Unlock()
	s.End("s1")

	first, _ := s.Get("s1")
	if first.Duration != 5 {
		t.Fatalf("首次 End 时长应为 5s，实际 %d", first.Duration)
	}

	// 再把开始时间往前拨 5 秒（模拟又过去 5 秒），第二次 End 不能改写记录。
	s.mu.Lock()
	s.sessions["s1"].StartedAt = time.Now().Add(-10 * time.Second)
	s.mu.Unlock()
	s.End("s1")

	second, _ := s.Get("s1")
	if !second.EndedAt.Equal(first.EndedAt) || second.Duration != first.Duration {
		t.Errorf("第二次 End 应为 no-op，实际 ended %v→%v duration %d→%d",
			first.EndedAt, second.EndedAt, first.Duration, second.Duration)
	}
}

// 服务端重启时落盘的 Live 记录必须被扫尾：PTY、hub 与宽限期定时器全在内存，
// 重启后不可能还活着。结束时间未知，按开始时间落 End、时长记 0，不编造。
func TestLoadSweepsStaleLiveSessions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	stale := &Session{ID: "live-1", AgentID: "a1", Hostname: "h1", Operator: "alice", StartedAt: now.Add(-time.Hour), Live: true}
	done := &Session{ID: "done-1", AgentID: "a1", Hostname: "h1", Operator: "alice", StartedAt: now.Add(-2 * time.Hour), EndedAt: now.Add(-time.Hour), Duration: 3600}
	data, err := json.Marshal([]*Session{stale, done})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), data, 0o600); err != nil {
		t.Fatalf("write sessions.json: %v", err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	got, ok := s.Get("live-1")
	if !ok {
		t.Fatal("扫尾后旧 Live 记录应仍在")
	}
	if got.Live {
		t.Error("重启后旧 Live 记录应被置为已结束")
	}
	if !got.EndedAt.Equal(got.StartedAt) || got.Duration != 0 {
		t.Errorf("扫尾应按开始时间落 End、时长 0，实际 ended=%v started=%v duration=%d", got.EndedAt, got.StartedAt, got.Duration)
	}

	// 已结束的记录原样不动。
	kept, _ := s.Get("done-1")
	if kept.Live || kept.Duration != 3600 || !kept.EndedAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("已结束记录不应被扫尾改写，实际 %+v", kept)
	}

	// 扫尾结果应已落盘：重新加载仍是终态。
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reload NewStore: %v", err)
	}
	reloaded, _ := s2.Get("live-1")
	if reloaded.Live {
		t.Error("扫尾结果应已持久化，重新加载后仍应为已结束")
	}
}

// 对不存在的 sid 调 End 不应 panic、不应凭空建记录。
func TestEndUnknownSessionNoop(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.End("no-such-session")
	if _, ok := s.Get("no-such-session"); ok {
		t.Error("End 未知 sid 不应创建记录")
	}
}

// Start 必须 create-if-absent：记录在首次 WS 接入时创建，重连或第二位观众
// 接入时再调 Start 不能重置 StartedAt、也不能覆盖第一次的归属信息，更不能
// 复活一条已被 admin 删除的记录。
func TestStartCreateIfAbsent(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.Start("s1", "agent-1", "host-1", "alice")

	// 让 StartedAt 可分辨：把第一次的开始时间往前拨 5 秒。
	s.mu.Lock()
	s.sessions["s1"].StartedAt = time.Now().Add(-5 * time.Second)
	s.mu.Unlock()

	// 第二次 Start 带完全不同的归属信息：必须原样保留第一次的记录。
	s.Start("s1", "agent-2", "host-2", "bob")
	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("第二次 Start 后记录应仍在")
	}
	if got.AgentID != "agent-1" || got.Hostname != "host-1" || got.Operator != "alice" {
		t.Errorf("第二次 Start 不应覆盖已有记录，实际 %+v", got)
	}
	if time.Since(got.StartedAt) < 4*time.Second {
		t.Errorf("第二次 Start 不应重置 StartedAt，实际 %v", got.StartedAt)
	}
	if !got.Live {
		t.Error("第二次 Start 不应改变 Live")
	}
}
