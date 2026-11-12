package ws

import (
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"watchman/proto/agentpb"
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

// PTY 退出（空输出）时必须立即终结审计记录，不能等浏览器关 socket——标签页
// 开着不关时记录会永远停在 0s 进行中；普通输出则不能碰 End。
func TestOnTermOutputEndsSession(t *testing.T) {
	resetHubs()
	h, _ := getOrCreateHub("s-end", "agent-1")

	var mu sync.Mutex
	calls := 0
	var lastSid string
	onEnd := func(sid string) {
		mu.Lock()
		calls++
		lastSid = sid
		mu.Unlock()
	}
	endCalls := func() (int, string) {
		mu.Lock()
		defer mu.Unlock()
		return calls, lastSid
	}

	// 普通输出：只广播，不 End。
	h.onTermOutput("s-end", &agentpb.TerminalOutput{SessionId: "s-end", Data: []byte("hello")}, onEnd)
	if n, _ := endCalls(); n != 0 {
		t.Errorf("普通输出不应调 onEnd，实际调了 %d 次", n)
	}

	// 空输出 = PTY 已退出：End 恰好一次，且 sid 正确。
	h.onTermOutput("s-end", &agentpb.TerminalOutput{SessionId: "s-end", Data: []byte{}}, onEnd)
	if n, sid := endCalls(); n != 1 || sid != "s-end" {
		t.Errorf("PTY 退出应恰好 End 1 次(sid=s-end)，实际 %d 次 sid=%q", n, sid)
	}

	// onEnd 为空时不能 panic（无审计 store 的 handler）。
	h.onTermOutput("s-end", &agentpb.TerminalOutput{SessionId: "s-end", Data: []byte{}}, nil)
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

// CloseSession 立即拆掉会话、跳过宽限期：hub 移除、待触发的 reap 定时器停掉，
// 之后再调与 detachClient 都是 no-op，teardown 不会被触发两次。
func TestCloseSession(t *testing.T) {
	resetHubs()
	teardownCalled := false
	teardown := func() { teardownCalled = true }

	// 挂一个假客户端再 detach，让 reap 定时器先武装起来。
	h, live := getOrCreateHub("sid-close-1", "agent-1")
	if live {
		t.Fatal("首个接入应报告无 PTY")
	}
	c1 := &websocket.Conn{}
	h.mu.Lock()
	h.clients[c1] = "owner"
	h.mu.Unlock()
	detachClient("sid-close-1", c1, teardown)
	h.mu.Lock()
	armed := h.reapTimer != nil
	h.mu.Unlock()
	if !armed {
		t.Fatal("最后一位客户端离开后应武装 reap 定时器")
	}

	if !CloseSession("sid-close-1") {
		t.Fatal("CloseSession 应对存活会话返回 true")
	}
	h.mu.Lock()
	armed = h.reapTimer != nil
	h.mu.Unlock()
	if armed {
		t.Error("CloseSession 应停掉待触发的 reap 定时器")
	}
	hubMu.Lock()
	_, still := activeHubs["sid-close-1"]
	hubMu.Unlock()
	if still {
		t.Error("CloseSession 后 hub 应已移除")
	}
	// 第二次调与事后 detach 都是 no-op，teardown 不应被触发。
	if CloseSession("sid-close-1") {
		t.Error("第二次 CloseSession 应对不存在的会话返回 false")
	}
	detachClient("sid-close-1", c1, teardown)
	if teardownCalled {
		t.Error("CloseSession 后定时器与 detach 都不应再触发 teardown")
	}

	// 零客户端的 hub 同样能立即关闭。
	getOrCreateHub("sid-close-2", "agent-1")
	if !CloseSession("sid-close-2") {
		t.Error("零客户端的 CloseSession 也应返回 true")
	}
}
