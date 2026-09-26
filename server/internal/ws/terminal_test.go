package ws

import (
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// resetHubs clears global state so tests do not leak into each other.
func resetHubs() {
	hubMu.Lock()
	for k, h := range activeHubs {
		h.mu.Lock()
		if h.reapTimer != nil {
			h.reapTimer.Stop()
		}
		h.mu.Unlock()
		delete(activeHubs, k)
	}
	hubMu.Unlock()
}

// The first attach must report "no live PTY" so the caller opens one; a second
// attach to the same session must report "live" so it reattaches instead of
// spawning a second shell over the first.
func TestSecondAttachReusesPTY(t *testing.T) {
	resetHubs()
	c1 := &websocket.Conn{}
	c2 := &websocket.Conn{}

	h1, live1 := getOrCreateHub("s1", "agent-1")
	if live1 {
		t.Error("首个客户端接入时不应报告已有 PTY")
	}
	h1.mu.Lock()
	h1.clients[c1] = "owner"
	h1.mu.Unlock()

	h2, live2 := getOrCreateHub("s1", "agent-1")
	if !live2 {
		t.Error("同一会话的第二个客户端应复用已有 PTY")
	}
	if h1 != h2 {
		t.Error("同一 sid 必须拿到同一个 hub")
	}
	h2.mu.Lock()
	h2.clients[c2] = "view"
	n := len(h2.clients)
	h2.mu.Unlock()
	if n != 2 {
		t.Errorf("hub 应有 2 个客户端，实际 %d", n)
	}
}

// The whole point of the grace period: a browser that drops and comes back
// within it finds its shell alive, and teardown never fires.
func TestReconnectWithinGraceKeepsPTY(t *testing.T) {
	resetHubs()
	// 用极短宽限期跑完整流程，避免测试等 60 秒。
	orig := sessionGraceForTest
	sessionGraceForTest = 300 * time.Millisecond
	defer func() { sessionGraceForTest = orig }()

	conn := &websocket.Conn{}
	h, _ := getOrCreateHub("s2", "agent-1")
	h.mu.Lock()
	h.clients[conn] = "owner"
	h.mu.Unlock()

	var mu sync.Mutex
	torn := false
	detachClient("s2", conn, func() {
		mu.Lock()
		torn = true
		mu.Unlock()
	})

	// 宽限期内重连
	time.Sleep(100 * time.Millisecond)
	h2, live := getOrCreateHub("s2", "agent-1")
	if !live {
		t.Fatal("宽限期内重连应复用原 PTY，而不是新开一个")
	}
	conn2 := &websocket.Conn{}
	h2.mu.Lock()
	h2.clients[conn2] = "owner"
	h2.mu.Unlock()

	// 等过原定的宽限期，teardown 不应触发
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if torn {
		t.Error("重连成功后不应再销毁 PTY")
	}
	hubMu.Lock()
	_, still := activeHubs["s2"]
	hubMu.Unlock()
	if !still {
		t.Error("会话 hub 不应被移除")
	}
}

// Nobody comes back: the PTY must be reaped exactly once and the hub removed,
// otherwise shells and session index entries pile up forever.
func TestGraceExpiryTearsDownOnce(t *testing.T) {
	resetHubs()
	orig := sessionGraceForTest
	sessionGraceForTest = 200 * time.Millisecond
	defer func() { sessionGraceForTest = orig }()

	conn := &websocket.Conn{}
	h, _ := getOrCreateHub("s3", "agent-1")
	h.mu.Lock()
	h.clients[conn] = "owner"
	h.mu.Unlock()

	var mu sync.Mutex
	calls := 0
	detachClient("s3", conn, func() {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	time.Sleep(600 * time.Millisecond)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("宽限期到期应恰好销毁 1 次，实际 %d 次", got)
	}
	hubMu.Lock()
	_, still := activeHubs["s3"]
	hubMu.Unlock()
	if still {
		t.Error("到期后 hub 应从 activeHubs 移除，否则内存泄漏")
	}
}

// Two viewers sharing a session: one leaving must not disturb the other, and no
// teardown timer may be armed while anyone is still attached.
func TestOneOfTwoClientsLeavingKeepsSession(t *testing.T) {
	resetHubs()
	orig := sessionGraceForTest
	sessionGraceForTest = 150 * time.Millisecond
	defer func() { sessionGraceForTest = orig }()

	c1, c2 := &websocket.Conn{}, &websocket.Conn{}
	h, _ := getOrCreateHub("s4", "agent-1")
	h.mu.Lock()
	h.clients[c1] = "owner"
	h.clients[c2] = "view"
	h.mu.Unlock()

	var mu sync.Mutex
	torn := false
	detachClient("s4", c1, func() {
		mu.Lock()
		torn = true
		mu.Unlock()
	})
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if torn {
		t.Error("还有观众在线时不应销毁会话")
	}
	h.mu.Lock()
	n := len(h.clients)
	armed := h.reapTimer != nil
	h.mu.Unlock()
	if n != 1 {
		t.Errorf("应剩 1 个客户端，实际 %d", n)
	}
	if armed {
		t.Error("仍有客户端在线时不应挂起销毁定时器")
	}
}
