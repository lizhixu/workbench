package cert

import (
	"strings"
	"sync"
	"testing"
)

func TestCertLoggerBasicAndStages(t *testing.T) {
	l := newCertLogger("test_target", nil)
	l.Log("准备账户", "正在加载 ACME 账户")
	l.Logf("创建订单", "提交订单包含 %d 个域名", 2)
	l.Warn("CA验证", "CA 校验发生警告")
	l.Warnf("清理挑战", "释放端口 %d", 80)

	out := l.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), out)
	}

	expectedPrefixes := []string{
		"[准备账户] 正在加载 ACME 账户",
		"[创建订单] 提交订单包含 2 个域名",
		"[WARN:CA验证] CA 校验发生警告",
		"[WARN:清理挑战] 释放端口 80",
	}

	for i, exp := range expectedPrefixes {
		if !strings.Contains(lines[i], exp) {
			t.Errorf("line %d does not contain %q: got %q", i, exp, lines[i])
		}
		// Check that timestamp format [YYYY-MM-DD HH:mm:ss] is present
		if !strings.HasPrefix(lines[i], "[20") {
			t.Errorf("line %d missing timestamp prefix: %q", i, lines[i])
		}
	}
}

func TestCertLoggerBoundedCap(t *testing.T) {
	l := newCertLogger("test_cap", nil)

	// Write more than maxCertLogBytes (64KB)
	chunk := strings.Repeat("A", 1024)
	for i := 0; i < 70; i++ {
		l.Logf("测试", "chunk %02d: %s", i, chunk)
	}

	out := l.String()
	if len(out) > maxCertLogBytes+2048 {
		t.Fatalf("expected log length bounded near %d, got %d", maxCertLogBytes, len(out))
	}
	if !strings.Contains(out, "...[log truncated]...\n") {
		t.Fatalf("expected truncation notice in log, got:\n%s", out[:200])
	}
	if !strings.Contains(out, "chunk 00:") {
		t.Errorf("expected initial chunk 00 in output")
	}
}

func TestCertLoggerConcurrency(t *testing.T) {
	l := newCertLogger("test_concurrent", nil)
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Logf("并发", "worker %d iteration %d", workerID, j)
				_ = l.String()
			}
		}(i)
	}

	wg.Wait()
	out := l.String()
	if len(out) == 0 {
		t.Fatal("expected non-empty log output after concurrent writes")
	}
}
