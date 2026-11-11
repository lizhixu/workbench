package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"watchman/internal/version"
	"watchman/server/internal/vault"
)

const manifestName = ".watchman-backup.json"
const pendingName = "restore-pending.json"

type archiveFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type archiveManifest struct {
	Format        string        `json:"format"`
	Version       int           `json:"format_version"`
	ServerVersion string        `json:"server_version"`
	Files         []archiveFile `json:"files"`
}
type restoreFile struct {
	archiveFile
	Existed bool `json:"existed"`
}
type pendingRestore struct {
	ID     string        `json:"id"`
	Phase  string        `json:"phase"` // ready | applying | committed | failed
	Actor  string        `json:"actor"`
	Files  []restoreFile `json:"files"`
	Result RestoreResult `json:"result"`
}

func writeManifest(tw *tar.Writer, manifest archiveManifest) error {
	manifest.ServerVersion = version.Get()
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(raw) > 32<<20 {
		return fmt.Errorf("备份清单超过 32 MiB 上限")
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0600, Size: int64(len(raw)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = tw.Write(raw)
	return err
}

func (s *Store) pendingPath() string { return filepath.Join(s.backupDir, pendingName) }
func (s *Store) hasPending() bool {
	if s.dataDir == "" {
		return false
	}
	_, err := os.Stat(s.pendingPath())
	return !os.IsNotExist(err)
}

func (s *Store) prepareFile(name, actor, passphrase string) (RestoreResult, error) {
	p, err := s.Path(name)
	if err != nil {
		return RestoreResult{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return RestoreResult{}, err
	}
	defer f.Close()
	var expected string
	for _, m := range s.List() {
		if m.Name == name {
			expected = m.SHA256
			break
		}
	}
	return s.prepare(f, actor, "restore-from:"+name, expected, passphrase)
}

func (s *Store) prepare(r io.Reader, actor, note, expected, passphrase string) (result RestoreResult, err error) {
	if s.dataDir == "" {
		return result, ErrDisabled
	}
	if s.hasPending() {
		return result, ErrPending
	}
	dir, err := os.MkdirTemp(s.backupDir, ".restore-")
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()
	stage := filepath.Join(dir, "files")
	if err = os.Mkdir(stage, 0700); err != nil {
		return result, err
	}
	hasher := sha256.New()
	compressed := &io.LimitedReader{R: r, N: MaxArchiveBytes + 1}
	gz, err := gzip.NewReader(io.TeeReader(compressed, hasher))
	if err != nil {
		return result, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	// Also bound tar headers and trailing padding, not only file payloads.
	unpacked := &io.LimitedReader{R: gz, N: MaxArchiveBytes + 1}
	tr := tar.NewReader(unpacked)
	files := make(map[string]archiveFile)
	seen := make(map[string]bool)
	var manifest *archiveManifest
	var total int64
	entries := 0
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return result, fmt.Errorf("归档损坏: %w", nextErr)
		}
		entries++
		if entries > MaxRestoreEntries+1 {
			return result, fmt.Errorf("归档条目超过上限")
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return result, fmt.Errorf("refusing 非普通文件: %s", hdr.Name)
		}
		p, pathErr := safeJoin(stage, hdr.Name)
		if pathErr != nil {
			return result, pathErr
		}
		rel, _ := filepath.Rel(stage, p)
		name := filepath.ToSlash(rel)
		key := strings.ToLower(name) // portable collision check, including Windows
		if seen[key] {
			return result, fmt.Errorf("refusing 重复归档条目: %s", name)
		}
		seen[key] = true
		if name == manifestName {
			if hdr.Typeflag != tar.TypeReg || hdr.Size > 32<<20 {
				return result, fmt.Errorf("备份清单无效")
			}
			var m archiveManifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return result, fmt.Errorf("备份清单损坏")
			}
			manifest = &m
			continue
		}
		first := strings.ToLower(strings.SplitN(name, "/", 2)[0])
		if first == "bin" || first == "volume-uploads" {
			// Older archives included replaceable executables and upload
			// staging. Discard those while still validating the gzip stream.
			continue
		}
		if excluded(name) {
			return result, fmt.Errorf("refusing 恢复保留目录或暂存文件: %s", name)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(p, 0700); err != nil {
				return result, err
			}
			continue
		}
		if hdr.Size < 0 || hdr.Size > MaxRestoreBytes-total {
			return result, fmt.Errorf("归档数据超过 64 GiB 恢复上限")
		}
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return result, err
		}
		h := sha256.New()
		if _, err := writeFile(p, io.TeeReader(tr, h), hdr.Size); err != nil {
			return result, err
		}
		files[name] = archiveFile{Path: name, Size: hdr.Size, SHA256: hex.EncodeToString(h.Sum(nil))}
		total += hdr.Size
		s.progress("validating", len(files), total)
	}
	// tar EOF occurs before gzip EOF. Drain and validate CRC/ISIZE explicitly;
	// permit only zero padding after the tar terminator.
	buf := make([]byte, 32<<10)
	for {
		n, readErr := unpacked.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return result, fmt.Errorf("归档尾部含额外数据")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return result, fmt.Errorf("归档完整性校验失败: %w", readErr)
		}
	}
	if compressed.N <= 0 || unpacked.N <= 0 {
		return result, fmt.Errorf("归档超过容量上限")
	}
	if expected != "" && !strings.EqualFold(expected, hex.EncodeToString(hasher.Sum(nil))) {
		return result, fmt.Errorf("归档 SHA256 不匹配，文件可能损坏")
	}
	if len(files) == 0 {
		return result, fmt.Errorf("归档没有可恢复的数据")
	}
	if manifest != nil {
		if manifest.Format != "watchman-control-backup" || manifest.Version != 1 || len(manifest.Files) != len(files) {
			return result, fmt.Errorf("备份清单格式或文件数量不匹配")
		}
		checked := map[string]bool{}
		for _, f := range manifest.Files {
			got, ok := files[f.Path]
			if !ok || checked[f.Path] || got.Size != f.Size || got.SHA256 != f.SHA256 {
				return result, fmt.Errorf("备份文件校验失败: %s", f.Path)
			}
			checked[f.Path] = true
		}
	} else {
		// Legacy backups have no manifest. Require a known control-plane file
		// so arbitrary tar files cannot be mistaken for a Watchman backup.
		known := false
		for _, p := range []string{"users.json", "agents.json", "settings.json", "vault.json"} {
			if _, ok := files[p]; ok {
				known = true
			}
		}
		if !known {
			return result, fmt.Errorf("未识别为控制端备份归档")
		}
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".json") {
			if err := validateJSONFile(filepath.Join(stage, filepath.FromSlash(f.Path))); err != nil {
				return result, fmt.Errorf("配置 %s 无效: %w", f.Path, err)
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(stage, "control-backup-policy.json")); err == nil {
		var p Policy
		if json.Unmarshal(raw, &p) != nil {
			return result, fmt.Errorf("自动备份策略格式错误")
		}
		if _, err := time.Parse("15:04", p.Time); err != nil || p.Retention < 1 || p.Retention > 100 {
			return result, fmt.Errorf("自动备份策略无效")
		}
	}
	if err := s.validateRestoreKeys(stage, passphrase); err != nil {
		return result, err
	}
	// Key migration may add a missing legacy secrets file. Include it in the
	// transaction digest so restart verifies exactly the prepared generation.
	if _, statErr := os.Stat(filepath.Join(stage, secretsFile)); statErr == nil {
		f, hashErr := hashFile(stage, secretsFile)
		if hashErr != nil {
			return result, hashErr
		}
		files[secretsFile] = f
	}
	pending := pendingRestore{ID: filepath.Base(dir), Phase: "ready", Actor: actor,
		Result: RestoreResult{Files: len(files), Bytes: total, RestartRequired: true, Pending: true, Legacy: manifest == nil}}
	for _, f := range files {
		pending.Files = append(pending.Files, restoreFile{archiveFile: f})
	}
	sort.Slice(pending.Files, func(i, j int) bool { return pending.Files[i].Path < pending.Files[j].Path })
	if err := atomicJSON(s.pendingPath(), pending); err != nil {
		return result, err
	}
	s.log.Info("control restore prepared", "files", len(files), "by", actor, "source", note)
	return pending.Result, nil
}

func validateJSONFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	first, err := dec.Token()
	if err != nil {
		return err
	}
	if first == nil {
		if _, err := dec.Token(); err != io.EOF {
			return fmt.Errorf("配置含多个 JSON 文档")
		}
		return nil
	}
	delim, ok := first.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return fmt.Errorf("配置必须为 JSON 对象或数组")
	}
	if filepath.Base(p) == "users.json" && delim != '[' {
		return fmt.Errorf("用户配置必须为数组")
	}
	if filepath.Base(p) == "agents.json" && delim != '{' {
		return fmt.Errorf("主机配置必须为对象")
	}
	depth := 1
	for depth > 0 {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := token.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
			if depth > 100 {
				return fmt.Errorf("配置嵌套过深")
			}
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("配置含多个 JSON 文档")
	}
	return nil
}

func hashFile(root, name string) (archiveFile, error) {
	p, err := safeJoin(root, name)
	if err != nil {
		return archiveFile{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return archiveFile{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return archiveFile{Path: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, err
}

func (s *Store) validateRestoreKeys(stage, passphrase string) error {
	var keys ServerSecrets
	raw, err := os.ReadFile(filepath.Join(stage, secretsFile))
	if err == nil {
		if json.Unmarshal(raw, &keys) != nil || keys.JWTKey == "" || keys.VaultPass == "" {
			return fmt.Errorf("归档内控制端密钥文件无效")
		}
	} else if os.IsNotExist(err) {
		raw, err = os.ReadFile(filepath.Join(s.dataDir, secretsFile))
		if err == nil {
			if json.Unmarshal(raw, &keys) != nil {
				return fmt.Errorf("本机密钥文件无效")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if passphrase != "" {
			keys.VaultPass = passphrase
		}
	} else {
		return err
	}
	vaultPath := filepath.Join(stage, "vault.json")
	if _, err := os.Stat(vaultPath); os.IsNotExist(err) {
		vaultPath = filepath.Join(s.dataDir, "vault.json")
	}
	if raw, err := os.ReadFile(vaultPath); err == nil {
		if err := vault.ValidateSecrets(raw, keys.VaultPass); err != nil {
			return fmt.Errorf("归档凭据无法解密；旧归档请填写原服务器 Vault 密钥: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if keys.VaultPass != "" && keys.JWTKey != "" {
		return atomicJSON(filepath.Join(stage, secretsFile), keys)
	}
	return nil
}

// safeTarget refuses pre-existing symlink ancestors as well as tar-slip names.
func safeTarget(root, name string) (string, error) {
	p, err := safeJoin(root, name)
	if err != nil {
		return "", err
	}
	cur := p
	for cur != root {
		st, err := os.Lstat(cur)
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing 符号链接路径: %s", name)
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		cur = filepath.Dir(cur)
	}
	return p, nil
}

func (s *Store) readPending() (*pendingRestore, error) {
	raw, err := os.ReadFile(s.pendingPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p pendingRestore
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(p.ID, ".restore-") || filepath.Base(p.ID) != p.ID || strings.ContainsAny(p.ID, `/\`) {
		return nil, fmt.Errorf("恢复事务目录无效")
	}
	for _, f := range p.Files {
		if _, err := safeJoin(s.dataDir, f.Path); err != nil {
			return nil, err
		}
		if excluded(f.Path) {
			return nil, fmt.Errorf("恢复事务包含保留路径")
		}
	}
	return &p, nil
}

// ApplyPending MUST run before any business store or background worker starts.
// A write-ahead journal makes rollback repeatable even after a process crash.
func (s *Store) ApplyPending() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	p, err := s.readPending()
	if err != nil || p == nil {
		return err
	}
	dir := filepath.Join(s.backupDir, p.ID)
	stage := filepath.Join(dir, "files")
	undo := filepath.Join(dir, "undo")
	root, err := filepath.Abs(s.dataDir)
	if err != nil {
		return err
	}
	if p.Phase == "committed" {
		return s.finishRestore(p, nil)
	}
	if p.Phase == "applying" {
		if err := s.rollback(root, undo, p); err != nil {
			return fmt.Errorf("恢复回滚未完成，拒绝启动: %w", err)
		}
		return s.finishRestore(p, fmt.Errorf("上次恢复被中断，已自动回滚"))
	}
	if p.Phase != "ready" {
		return fmt.Errorf("未知恢复事务状态 %q", p.Phase)
	}
	// Live credentials may have changed since preparation. Revalidate the
	// effective vault/key pair before the first replacement, including partial
	// legacy archives that keep the current vault file.
	if err := s.validateRestoreKeys(stage, ""); err != nil {
		return s.finishRestore(p, err)
	}
	for i, f := range p.Files {
		got, err := hashFile(stage, f.Path)
		if err != nil || got.Size != f.Size || got.SHA256 != f.SHA256 {
			return s.finishRestore(p, fmt.Errorf("暂存文件校验失败: %s", f.Path))
		}
		target, err := safeTarget(root, f.Path)
		if err != nil {
			return s.finishRestore(p, err)
		}
		st, err := os.Lstat(target)
		if err == nil {
			if !st.Mode().IsRegular() {
				return s.finishRestore(p, fmt.Errorf("目标不是普通文件: %s", f.Path))
			}
			p.Files[i].Existed = true
		} else if !os.IsNotExist(err) {
			return s.finishRestore(p, err)
		}
	}
	safety, err := s.create(p.Actor, "恢复前自动安全备份", "safety")
	if err != nil {
		return s.finishRestore(p, fmt.Errorf("恢复前安全备份失败: %w", err))
	}
	p.Result.SafetyCopy = safety.Name
	p.Phase = "applying"
	if err := atomicJSON(s.pendingPath(), p); err != nil {
		return err
	}
	for i, f := range p.Files {
		target, _ := safeJoin(root, f.Path)
		src, _ := safeJoin(stage, f.Path)
		old, _ := safeJoin(undo, f.Path)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			break
		}
		if f.Existed {
			if err = os.MkdirAll(filepath.Dir(old), 0700); err != nil {
				break
			}
			if err = s.rename(target, old); err != nil {
				break
			}
		}
		if err = s.rename(src, target); err != nil {
			break
		}
		s.progress("applying", i+1, 0)
	}
	if err != nil {
		if rollbackErr := s.rollback(root, undo, p); rollbackErr != nil {
			return fmt.Errorf("恢复失败且回滚未完成，拒绝启动: %v; %w", err, rollbackErr)
		}
		return s.finishRestore(p, fmt.Errorf("恢复失败，已自动回滚: %w", err))
	}
	p.Phase = "committed"
	if err := atomicJSON(s.pendingPath(), p); err != nil {
		return fmt.Errorf("写入恢复提交标记失败: %w", err)
	}
	return s.finishRestore(p, nil)
}

func (s *Store) rollback(root, undo string, p *pendingRestore) error {
	for i := len(p.Files) - 1; i >= 0; i-- {
		f := p.Files[i]
		target, err := safeTarget(root, f.Path)
		if err != nil {
			return err
		}
		old, _ := safeJoin(undo, f.Path)
		if _, err := os.Stat(old); err == nil {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Rename(old, target); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		} else if !f.Existed {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (s *Store) finishRestore(p *pendingRestore, cause error) error {
	s.mu.Lock()
	s.task.Kind = "restore"
	s.task.Phase = "complete"
	s.task.Status = "success"
	s.task.Error = ""
	s.task.FinishedAt = time.Now()
	p.Result.Pending = false
	p.Result.RestartRequired = false
	s.task.Result = &p.Result
	if cause != nil {
		s.task.Status = "failed"
		s.task.Error = cause.Error()
		s.log.Error("restore rolled back", "err", cause)
	}
	err := s.saveTaskLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.Remove(s.pendingPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.backupDir, p.ID)); err != nil {
		return err
	}
	return s.loadOperations()
}

func (s *Store) CancelPending() error {
	if !s.opMu.TryLock() {
		return ErrBusy
	}
	defer s.opMu.Unlock()
	p, err := s.readPending()
	if err != nil || p == nil {
		return err
	}
	if p.Phase != "ready" {
		return ErrBusy
	}
	if err := os.Remove(s.pendingPath()); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.backupDir, p.ID)); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.task.Status = "cancelled"
	s.task.Phase = "complete"
	s.task.Result = nil
	s.task.FinishedAt = time.Now()
	return s.saveTaskLocked()
}
