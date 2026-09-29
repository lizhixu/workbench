// Package binval provides executable binary validation, architecture verification,
// and smoke-testing to prevent corrupt, mismatched, or broken binaries from being
// installed during upgrades.
package binval

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/pe"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Default limits for binary size sanity checks.
const (
	MinServerBinarySize = 5 * 1024 * 1024   // 5 MB (server has embedded web UI)
	MinAgentBinarySize  = 1 * 1024 * 1024   // 1 MB
	MaxBinarySize       = 300 * 1024 * 1024 // 300 MB
)

// FileSha256 computes the hex-encoded SHA-256 checksum of a file.
func FileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file for hash: %w", err)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", fmt.Errorf("read file for hash: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ValidateFormat checks whether the binary at path is a valid executable for
// the target operating system and CPU architecture.
func ValidateFormat(path string, targetOS, targetArch string, minSize int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("无法读取文件信息: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("目标路径为目录，非可执行文件")
	}

	size := info.Size()
	if minSize <= 0 {
		minSize = MinAgentBinarySize
	}
	if size < minSize {
		return fmt.Errorf("文件体积异常 (%d 字节)，低于安全下限 %d 字节，可能已损坏或被截断", size, minSize)
	}
	if size > MaxBinarySize {
		return fmt.Errorf("文件体积异常 (%d 字节)，超过允许的最大安全上限 (300MB)", size)
	}

	targetOS = strings.ToLower(strings.TrimSpace(targetOS))
	targetArch = strings.ToLower(strings.TrimSpace(targetArch))
	if targetOS == "" {
		targetOS = runtime.GOOS
	}
	if targetArch == "" {
		targetArch = runtime.GOARCH
	}

	switch targetOS {
	case "linux":
		return validateLinuxELF(path, targetArch)
	case "windows":
		return validateWindowsPE(path, targetArch)
	default:
		// Other OS: ensure non-empty
		return nil
	}
}

func validateLinuxELF(path string, targetArch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("文件不是合法的 Linux ELF 可执行程序: %w", err)
	}
	defer f.Close()

	if f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("架构类别不符: 仅支持 64 位 ELF 程序 (ELFCLASS64)，当前文件类别为 %v", f.Class)
	}
	if f.Data != elf.ELFDATA2LSB {
		return fmt.Errorf("字节序不符: 仅支持 Little-Endian (LSB)，当前文件为 %v", f.Data)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("ELF 类型不符: 必须为可执行文件 (ET_EXEC) 或共享对象/PIE (ET_DYN)，当前类型为 %v", f.Type)
	}

	switch targetArch {
	case "amd64", "x86_64":
		if f.Machine != elf.EM_X86_64 {
			return fmt.Errorf("CPU 架构不匹配: 期望 x86_64 (amd64)，上传程序架构为 %v", f.Machine)
		}
	case "arm64", "aarch64":
		if f.Machine != elf.EM_AARCH64 {
			return fmt.Errorf("CPU 架构不匹配: 期望 aarch64 (arm64)，上传程序架构为 %v", f.Machine)
		}
	default:
		return fmt.Errorf("不支持的目标架构: %s", targetArch)
	}

	return nil
}

func validateWindowsPE(path string, targetArch string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("文件不是合法的 Windows PE 可执行程序: %w", err)
	}
	defer f.Close()

	switch targetArch {
	case "amd64", "x86_64":
		if f.FileHeader.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
			return fmt.Errorf("CPU 架构不匹配: 期望 Windows x86_64 (amd64)，上传程序架构标识为 0x%X", f.FileHeader.Machine)
		}
	case "arm64", "aarch64":
		if f.FileHeader.Machine != pe.IMAGE_FILE_MACHINE_ARM64 {
			return fmt.Errorf("CPU 架构不匹配: 期望 Windows ARM64，上传程序架构标识为 0x%X", f.FileHeader.Machine)
		}
	default:
		return fmt.Errorf("不支持的目标架构: %s", targetArch)
	}

	return nil
}

// SmokeTest attempts a trial execution of the binary using "-version" to verify
// that the binary can actually run on the local operating system without dynamic
// linker, libc mismatch, or segmentation faults.
//
// If the target OS or arch does not match the current machine, SmokeTest is skipped.
func SmokeTest(ctx context.Context, path string, targetOS, targetArch string, expectedKeyword string) (string, error) {
	targetOS = strings.ToLower(strings.TrimSpace(targetOS))
	targetArch = strings.ToLower(strings.TrimSpace(targetArch))
	if targetOS == "" {
		targetOS = runtime.GOOS
	}
	if targetArch == "" {
		targetArch = runtime.GOARCH
	}

	// Smoke test can only be performed when running on the matching host OS/arch
	if targetOS != runtime.GOOS || targetArch != runtime.GOARCH {
		return "", nil
	}

	_ = os.Chmod(path, 0755)

	timeoutCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, path, "-version")
	out, err := cmd.CombinedOutput()
	outputStr := strings.TrimSpace(string(out))

	if err != nil {
		return outputStr, fmt.Errorf("二进制试运行自检失败 (-version 异常): %w (输出: %s)", err, outputStr)
	}

	if expectedKeyword != "" && !strings.Contains(outputStr, expectedKeyword) {
		return outputStr, fmt.Errorf("二进制试运行自检输出不符合预期 (未包含标识 %q，输出: %s)", expectedKeyword, outputStr)
	}

	return outputStr, nil
}
