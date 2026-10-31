package apps

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Uploaded restore files: the browser cannot reach the managed host, so an
// upload lands on the panel first and the agent pulls it back over a
// one-time token URL (the same curl-to-staging pattern the S3 restore uses,
// only the URL source differs). Files live under dataDir/volume-uploads,
// are removed when the restore run ends (whatever the outcome) or when the
// record is deleted, and are purged wholesale at panel startup — a restart
// kills the restore goroutine, so no staged file can still be in flight.

const (
	volumeUploadsDir = "volume-uploads"

	// volumeUploadMaxBytes caps one uploaded restore file. The bytes travel
	// browser -> panel -> agent, so the cap also bounds panel disk use.
	volumeUploadMaxBytes = int64(2) * 1000 * 1000 * 1000 // 2GB, exact

	// volumeUploadTokenTTL bounds the agent's download window; it matches
	// the S3 presign TTL the normal restore uses.
	volumeUploadTokenTTL = 2 * time.Hour
)

var errVolumeUploadTooLarge = errors.New("上传文件不能超过 2GB")

type volumeUploadToken struct {
	archiveID string
	appID     string
	source    string
	status    string
	expiresAt time.Time
}

// VolumeUploadStore stages operator-uploaded restore files on the panel and
// mints the one-time tokens the agent downloads them with.
type VolumeUploadStore struct {
	mu     sync.Mutex
	dir    string
	log    *slog.Logger
	tokens map[string]*volumeUploadToken
}

// NewVolumeUploadStore creates the staging directory and purges any files a
// previous process left behind: their restore goroutines died with it, so
// nothing can still legitimately download them.
func NewVolumeUploadStore(dataDir string, log *slog.Logger) (*VolumeUploadStore, error) {
	if log == nil {
		log = slog.Default()
	}
	dir := filepath.Join(dataDir, volumeUploadsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// MkdirAll does not tighten an existing directory. Uploads contain operator
	// data and must never inherit a permissive mode from an older installation.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("收紧上传目录权限失败: %w", err)
	}
	s := &VolumeUploadStore{dir: dir, log: log, tokens: make(map[string]*volumeUploadToken)}
	s.purgeStale()
	return s, nil
}

// purgeStale drops leftovers from a previous process. Failures are logged, not
// fatal: a locked or undeletable file (Windows AV, lingering handle) must not
// keep the console from booting, and stale files are inert because every
// upload gets a fresh random archive ID.
func (s *VolumeUploadStore) purgeStale() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		s.log.Warn("read volume upload staging dir failed; stale files not purged", "err", err)
		return
	}
	for _, en := range entries {
		if err := os.RemoveAll(filepath.Join(s.dir, en.Name())); err != nil {
			s.log.Warn("purge stale volume upload failed", "file", en.Name(), "err", err)
		}
	}
}

// Path returns the deterministic staging path for an archive's upload.
func (s *VolumeUploadStore) Path(archiveID string) string {
	return filepath.Join(s.dir, archiveID+".tar.gz")
}

// Save streams src to the archive's staging file, refusing anything over
// the cap (a limit+1 read detects overflow without trusting Content-Length).
// A partial file is removed on any error.
func (s *VolumeUploadStore) Save(archiveID string, src io.Reader) (int64, error) {
	return s.saveWithLimit(archiveID, src, volumeUploadMaxBytes)
}

func (s *VolumeUploadStore) saveWithLimit(archiveID string, src io.Reader, limit int64) (int64, error) {
	path := s.Path(archiveID)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(src, limit+1))
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return 0, err
	}
	if n > limit {
		os.Remove(path)
		return 0, errVolumeUploadTooLarge
	}
	return n, nil
}

// IssueToken mints a single-use download token for an archive's upload.
func (s *VolumeUploadStore) IssueToken(appID, archiveID string) (string, error) {
	return s.IssueTokenForRecord(appID, archiveID, VolumeArchiveSourceUpload, VolumeBackupUploaded)
}

// IssueTokenForRecord binds the bearer token to the complete archive identity
// and lifecycle state expected by the upload-download endpoint.
func (s *VolumeUploadStore) IssueTokenForRecord(appID, archiveID, source, status string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	s.mu.Lock()
	s.tokens[tok] = &volumeUploadToken{archiveID: archiveID, appID: appID, source: source, status: status, expiresAt: time.Now().Add(volumeUploadTokenTTL)}
	s.mu.Unlock()
	return tok, nil
}

// ConsumeToken validates and burns a token: one download per token, and an
// expired token is gone, not reusable. It is retained for package compatibility
// but production handlers use ConsumeTokenForRecord for complete binding.
func (s *VolumeUploadStore) ConsumeToken(tok string) (archiveID, appID string, ok bool) {
	return s.consumeToken(tok, "", "", "", "")
}

// peekToken returns the token binding without consuming it. Callers use this
// only to validate the archive's ownership and lifecycle before the final
// atomic ConsumeTokenForRecord call.
func (s *VolumeUploadStore) peekToken(tok string) (archiveID, appID, source, status string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, exists := s.tokens[tok]
	if !exists {
		return "", "", "", "", false
	}
	if time.Now().After(t.expiresAt) {
		delete(s.tokens, tok)
		return "", "", "", "", false
	}
	return t.archiveID, t.appID, t.source, t.status, true
}

// ConsumeTokenForRecord atomically validates every token binding before burning
// it. A failed validation does not consume the token, allowing the legitimate
// request to proceed after a rejected cross-app/archive/status attempt.
func (s *VolumeUploadStore) ConsumeTokenForRecord(tok, appID, archiveID, source, status string) bool {
	_, _, ok := s.consumeToken(tok, appID, archiveID, source, status)
	return ok
}

func (s *VolumeUploadStore) consumeToken(tok, wantAppID, wantArchiveID, wantSource, wantStatus string) (archiveID, appID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, exists := s.tokens[tok]
	if !exists {
		return "", "", false
	}
	if (wantAppID != "" && t.appID != wantAppID) || (wantArchiveID != "" && t.archiveID != wantArchiveID) ||
		(wantSource != "" && t.source != wantSource) || (wantStatus != "" && t.status != wantStatus) {
		return "", "", false
	}
	delete(s.tokens, tok)
	if time.Now().After(t.expiresAt) {
		return "", "", false
	}
	return t.archiveID, t.appID, true
}

// DropTokenByArchive invalidates any still-outstanding token for an archive
// (the restore ended without the agent downloading, or the record was
// deleted).
func (s *VolumeUploadStore) DropTokenByArchive(archiveID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, t := range s.tokens {
		if t.archiveID == archiveID {
			delete(s.tokens, tok)
		}
	}
}

// RemoveFile drops a staged upload; a missing file is fine (already served
// or already cleaned).
func (s *VolumeUploadStore) RemoveFile(archiveID string) {
	if s == nil {
		return
	}
	os.Remove(s.Path(archiveID))
}

// Serve streams an archive's staged upload to w. The file is removed after
// the transfer, whether it completed or broke — a failed run is retried by
// re-uploading, and the restore goroutine's terminal cleanup covers the
// same file.
func (s *VolumeUploadStore) Serve(w http.ResponseWriter, archiveID string) error {
	f, err := os.Open(s.Path(archiveID))
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+archiveID+`.tar.gz"`)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, f)
	// Close before removing: Windows refuses to delete an open file, which
	// would leave the staged archive behind after every successful transfer.
	f.Close()
	os.Remove(s.Path(archiveID))
	return err
}

// uploadRestoreBaseURL accepts only a bound panel domain (resolved by
// settings.BaseURL). Request.Host is intentionally not a fallback: it is
// client-controlled behind reverse proxies and may point the agent at an
// attacker or an unreachable LAN.
func uploadRestoreBaseURL(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", errors.New("请先在系统设置 → 面板域名与证书绑定面板域名，再使用上传恢复")
	}
	u, err := url.Parse(configured)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("面板对外地址无效，请检查面板域名与 SSL 配置")
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("面板对外地址无效，请检查面板域名与 SSL 配置")
	}
	if portStr := u.Port(); portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 || p > 65535 {
			return "", errors.New("面板对外地址端口无效")
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}
