package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runWithDeadline fails the test if fn does not return in time. A mutator that
// persists while still holding the lock hangs forever and takes the whole
// credential store with it, so every mutator is checked for that.
func runWithDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s 未在 3 秒内返回（凭据金库已死锁）", what)
	}
}

// The full lifecycle must complete and leave the store usable.
func TestCredentialLifecycleDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "test-pass")
	if err != nil {
		t.Fatal(err)
	}

	var created *Credential
	runWithDeadline(t, "Create", func() {
		created, err = s.Create(&Credential{Name: "db-root", Type: "password", Username: "root", Secret: "s3cr3t"})
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Secret != "" {
		t.Error("Create 的返回值不应包含明文密钥")
	}

	runWithDeadline(t, "Update", func() {
		err = s.Update(created.ID, &Credential{Name: "db-root-renamed"})
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	runWithDeadline(t, "List", func() {
		if got := s.List(); len(got) != 1 || got[0].Name != "db-root-renamed" || got[0].Secret != "" {
			t.Errorf("List 结果不符: %+v", got)
		}
	})

	runWithDeadline(t, "Delete", func() {
		err = s.Delete(created.ID)
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The store must still serve reads and writes after a delete.
	runWithDeadline(t, "Delete 之后的 List", func() {
		if got := s.List(); len(got) != 0 {
			t.Errorf("删除后仍有 %d 条凭据", len(got))
		}
	})
	runWithDeadline(t, "Delete 之后的 Create", func() {
		_, err = s.Create(&Credential{Name: "second", Type: "password", Secret: "x"})
	})
	if err != nil {
		t.Fatalf("删除后再新增失败: %v", err)
	}
	runWithDeadline(t, "删除不存在的凭据", func() {
		if err := s.Delete("no-such-id"); err == nil {
			t.Error("删除不存在的凭据应报错")
		}
	})
}

// Secrets must be encrypted at rest and must survive a restart.
func TestSecretsEncryptedAtRestAndReloadable(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "pl4in-t3xt-secret"
	created, err := s.Create(&Credential{Name: "ssh", Type: "key", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "vault.json"))
	if err != nil {
		t.Fatalf("read vault.json: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Errorf("密钥以明文落盘:\n%s", raw)
	}

	s2, err := NewStore(dir, "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get(created.ID)
	if !ok {
		t.Fatal("重启后读不到凭据")
	}
	if got.Secret != secret {
		t.Errorf("重启后解密结果 = %q, 期望 %q", got.Secret, secret)
	}

	// A wrong passphrase must not silently expose the secret.
	s3, err := NewStore(dir, "wrong-pass")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := s3.Get(created.ID); ok && c.Secret == secret {
		t.Error("换了口令仍能解出明文密钥")
	}
}
