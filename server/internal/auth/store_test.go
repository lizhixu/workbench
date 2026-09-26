package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The console is internet-reachable, so short and obvious passwords must be
// rejected on both create and change.
func TestPasswordPolicy(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "k")
	if err != nil {
		t.Fatal(err)
	}

	for _, pw := range []string{"1", "short", "1234567", "password", "12345678"} {
		if _, err := s.Create("u_"+pw, pw, RoleViewer); err == nil {
			t.Errorf("Create 接受了弱口令 %q", pw)
		}
	}
	if _, err := s.Create("good", "Qa#12345678", RoleOperator); err != nil {
		t.Fatalf("合规口令被拒绝: %v", err)
	}
	if err := s.UpdatePassword("good", "Qa#12345678", "123"); err == nil {
		t.Error("UpdatePassword 接受了弱口令")
	}
	if err := s.ResetPassword("good", "x"); err == nil {
		t.Error("ResetPassword 接受了弱口令")
	}
	if err := s.ResetPassword("good", "Another#987654"); err != nil {
		t.Errorf("合规口令重置失败: %v", err)
	}
	if _, _, err := s.Authenticate("good", "Another#987654"); err != nil {
		t.Errorf("重置后无法登录: %v", err)
	}
}

// The hash must stay on disk (so restarts keep working) but never appear in a
// value that gets marshalled for an API response.
func TestHashPersistedButNotSerializedForAPI(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("carol", "Qa#12345678", RoleOperator); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("read users.json: %v", err)
	}
	if !strings.Contains(string(raw), "password_hash") || !strings.Contains(string(raw), "$2a$") {
		t.Fatalf("users.json 必须保存口令哈希，否则重启后无法登录:\n%s", raw)
	}

	// Re-open to prove the on-disk hash still authenticates.
	s2, err := NewStore(dir, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.Authenticate("carol", "Qa#12345678"); err != nil {
		t.Errorf("重启后登录失败，哈希未正确持久化: %v", err)
	}

	// The API-facing struct must not carry the hash.
	u, ok := s2.Get("carol")
	if !ok {
		t.Fatal("Get(carol) 失败")
	}
	out, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "password_hash") || strings.Contains(string(out), "$2a$") {
		t.Errorf("User 序列化后泄漏口令哈希: %s", out)
	}
	for _, lu := range s2.List() {
		out, _ := json.Marshal(lu)
		if strings.Contains(string(out), "$2a$") {
			t.Errorf("List() 元素泄漏口令哈希: %s", out)
		}
	}
}
