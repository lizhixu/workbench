package binval

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileSha256(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "binval-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "test.bin")
	content := []byte("hello watchman binary verification")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	sum, err := FileSha256(filePath)
	if err != nil {
		t.Fatalf("FileSha256 error: %v", err)
	}
	if len(sum) != 64 {
		t.Errorf("expected 64 hex characters, got %s", sum)
	}
}

func TestValidateFormat_RejectsSmallFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "binval-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "tiny.bin")
	if err := os.WriteFile(filePath, []byte("short"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	err = ValidateFormat(filePath, "linux", "amd64", 1024)
	if err == nil {
		t.Fatalf("expected error for small file, got nil")
	}
	if !strings.Contains(err.Error(), "低于安全下限") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidateFormat_RejectsNonExecutable(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "binval-test-*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create 2MB text file
	filePath := filepath.Join(tmpDir, "fake.bin")
	data := make([]byte, 2*1024*1024)
	copy(data, []byte("NOT_AN_ELF_OR_PE"))
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	err = ValidateFormat(filePath, "linux", "amd64", 1024)
	if err == nil {
		t.Fatalf("expected error for fake linux binary, got nil")
	}
	if !strings.Contains(err.Error(), "合法的 Linux ELF") {
		t.Errorf("unexpected error message: %v", err)
	}

	err = ValidateFormat(filePath, "windows", "amd64", 1024)
	if err == nil {
		t.Fatalf("expected error for fake windows binary, got nil")
	}
	if !strings.Contains(err.Error(), "合法的 Windows PE") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidateFormat_SelfExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("cannot resolve self executable")
	}

	// Should pass validation for host OS/arch with minSize=1
	err = ValidateFormat(exe, runtime.GOOS, runtime.GOARCH, 1)
	if err != nil {
		t.Errorf("self executable format validation failed: %v", err)
	}

	// Should fail if we ask for opposite arch or foreign OS
	wrongOS := "linux"
	if runtime.GOOS == "linux" {
		wrongOS = "windows"
	}
	err = ValidateFormat(exe, wrongOS, runtime.GOARCH, 1)
	if err == nil {
		t.Errorf("expected validation to fail for wrong OS %s, got nil", wrongOS)
	}
}

func TestSmokeTest_ForeignArchSkipped(t *testing.T) {
	ctx := context.Background()
	out, err := SmokeTest(ctx, "/path/does/not/matter", "linux", "arm64", "watchman")
	// If host is not linux/arm64, it should skip without error
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		if err != nil {
			t.Errorf("expected skip for foreign arch, got error: %v", err)
		}
		if out != "" {
			t.Errorf("expected empty output for foreign arch, got %s", out)
		}
	}
}
