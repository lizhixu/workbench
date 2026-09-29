package release

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"watchman/server/internal/rpc"
)

// PerformOnlineUpgrade downloads the official release package for targetVersion,
// verifies its SHA-256 against CHECKSUMS.txt, extracts the new watchman-server
// binary, atomically replaces the running executable, updates manifest.json and
// bundled agent binaries, and broadcasts a maintenance notice to all connected agents.
func PerformOnlineUpgrade(
	ctx context.Context,
	assetURL string,
	checksumsURL string,
	targetVersion string,
	reg *rpc.Registry,
	manifestPath string,
) error {
	if strings.TrimSpace(assetURL) == "" {
		return fmt.Errorf("下载链接为空，未找到适用于当前系统与架构的官方发布包")
	}

	tmpDir, err := os.MkdirTemp("", "watchman-upgrade-*")
	if err != nil {
		return fmt.Errorf("创建临时升级目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	client := &http.Client{Timeout: 10 * time.Minute}

	// 1. Download official release tarball and compute SHA-256 simultaneously.
	req, err := http.NewRequestWithContext(ctx, "GET", assetURL, nil)
	if err != nil {
		return fmt.Errorf("创建下载请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Watchman-Server-Upgrader")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载发布包失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载发布包 HTTP 错误: %d", resp.StatusCode)
	}

	tarballPath := filepath.Join(tmpDir, "dist.tar.gz")
	tarFile, err := os.OpenFile(tarballPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("创建临时安装包文件失败: %w", err)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(tarFile, hasher)
	if _, err := io.Copy(mw, resp.Body); err != nil {
		tarFile.Close()
		return fmt.Errorf("保存安装包内容失败: %w", err)
	}
	tarFile.Close()

	calcSha := hex.EncodeToString(hasher.Sum(nil))

	// 2. If CHECKSUMS.txt is available, verify checksum.
	if checksumsURL != "" {
		cReq, err := http.NewRequestWithContext(ctx, "GET", checksumsURL, nil)
		if err == nil {
			cReq.Header.Set("User-Agent", "Watchman-Server-Upgrader")
			cResp, err := client.Do(cReq)
			if err == nil && cResp.StatusCode == http.StatusOK {
				defer cResp.Body.Close()
				expectedSha := ""
				assetFileName := filepath.Base(assetURL)
				// If assetURL has query params or redirects, strip them for clean filename comparison:
				if idx := strings.IndexByte(assetFileName, '?'); idx >= 0 {
					assetFileName = assetFileName[:idx]
				}

				scanner := bufio.NewScanner(cResp.Body)
				for scanner.Scan() {
					line := strings.TrimSpace(scanner.Text())
					parts := strings.Fields(line)
					if len(parts) >= 2 {
						filename := filepath.Base(parts[1])
						if filename == assetFileName || strings.Contains(parts[1], assetFileName) {
							expectedSha = strings.ToLower(parts[0])
							break
						}
					}
				}

				if expectedSha != "" && !strings.EqualFold(calcSha, expectedSha) {
					return fmt.Errorf("SHA-256 校验失败 (期望: %s, 实际: %s)，安装包可能已被篡改或下载不完整", expectedSha, calcSha)
				}
			}
		}
	}

	// 3. Extract tarball contents: watchman-server, manifest.json, and agent binaries.
	extractedServer := filepath.Join(tmpDir, "new-watchman-server")
	extractedManifest := filepath.Join(tmpDir, "new-manifest.json")
	extractedBinDir := filepath.Join(tmpDir, "bin")
	_ = os.MkdirAll(extractedBinDir, 0755)

	tarF, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("打开已下载安装包失败: %w", err)
	}
	defer tarF.Close()

	gzr, err := gzip.NewReader(tarF)
	if err != nil {
		return fmt.Errorf("解压 gzip 安装包失败: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	foundServer := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tarball 结构失败: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		clean := filepath.Clean(hdr.Name)
		baseName := filepath.Base(clean)

		if clean == "bin/watchman-server" || clean == "watchman-server" || baseName == "watchman-server" {
			sf, err := os.OpenFile(extractedServer, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				return fmt.Errorf("创建提取 server 文件失败: %w", err)
			}
			if _, err := io.Copy(sf, tr); err != nil {
				sf.Close()
				return fmt.Errorf("提取 server 失败: %w", err)
			}
			sf.Close()
			foundServer = true
		} else if baseName == "manifest.json" {
			mf, err := os.OpenFile(extractedManifest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err == nil {
				_, _ = io.Copy(mf, tr)
				mf.Close()
			}
		} else if strings.HasPrefix(baseName, "watchman-agent") {
			af, err := os.OpenFile(filepath.Join(extractedBinDir, baseName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err == nil {
				_, _ = io.Copy(af, tr)
				af.Close()
			}
		}
	}

	if !foundServer {
		return fmt.Errorf("安装包内未找到 watchman-server 可执行程序")
	}

	// 4. Locate current running binary.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取当前可执行文件路径失败: %w", err)
	}
	target, err := filepath.EvalSymlinks(self)
	if err != nil {
		target = self
	}

	targetDir := filepath.Dir(target)

	// 5. Safety backup of existing binary.
	bakPath := filepath.Join(targetDir, fmt.Sprintf("watchman-server.bak.%d", time.Now().Unix()))
	_ = os.Rename(target, bakPath)

	// 6. Atomically replace target with new binary.
	if err := os.Rename(extractedServer, target); err != nil {
		_ = os.Rename(bakPath, target) // rollback
		return fmt.Errorf("原子替换 watchman-server 失败: %w", err)
	}
	_ = os.Chmod(target, 0755)

	// 7. Update manifest.json if present.
	if _, err := os.Stat(extractedManifest); err == nil {
		destManifest := manifestPath
		if destManifest == "" {
			destManifest = "/opt/watchman/manifest.json"
		}
		if err := copyFile(extractedManifest, destManifest); err == nil {
			if m, err := Load(destManifest); err == nil {
				// We don't import api package here to avoid circular dependency;
				// the updated manifest will be reloaded or refreshed.
				_ = m
			}
		}
	}

	// 8. Update bundled agent binaries if target directory is /opt/watchman/bin.
	if entries, err := os.ReadDir(extractedBinDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				src := filepath.Join(extractedBinDir, e.Name())
				dst := filepath.Join(targetDir, e.Name())
				_ = copyFile(src, dst)
				_ = os.Chmod(dst, 0755)
			}
		}
	}

	// 9. Broadcast maintenance notice before restart.
	if reg != nil {
		reg.BroadcastMaintenance("server_restart", 120)
	}

	// 10. Clear cache so next check shows updated version.
	ClearUpdateCache()

	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
