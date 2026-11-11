// Package backup implements control-plane backup and restore (AGENTS.md 5.1
// 数据自主可控, B.8.2 system/backup + system/restore).
//
// A backup is a gzipped tar of the data directory: user accounts, host
// registry, terminal recordings, metrics history, audit trail and settings.
// Restore validates into an isolated staging directory. Startup applies the
// prepared transaction before opening business stores, with a safety archive
// and a recoverable rollback journal. No live process data is overwritten.
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
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	archivePrefix  = "watchman-backup-"
	archiveSuffix  = ".tar.gz"
	backupsDirName = "backups"
	indexFileName  = "index.json"

	// Caps on what a restore will unpack, so a malformed or hostile archive
	// cannot exhaust the disk or the inode table.
	MaxRestoreBytes   int64 = 64 << 30
	MaxRestoreEntries       = 200000
	MaxArchiveBytes   int64 = MaxRestoreBytes + (1 << 30)
)

// ErrDisabled is returned when the store has no data directory to work with.
var ErrDisabled = errors.New("backup disabled: no data directory configured")
var ErrBusy = errors.New("已有备份或恢复任务正在执行")
var ErrPending = errors.New("已有待应用的恢复，请重启控制端或取消待恢复任务")

// Meta describes one archive on disk.
type Meta struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Files     int       `json:"files"`
	SHA256    string    `json:"sha256"`
	Note      string    `json:"note"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// Auto marks archives the server took on its own (scheduled runs and the
	// safety copy written just before a restore).
	Auto         bool   `json:"auto"`
	Kind         string `json:"kind,omitempty"` // manual | scheduled | safety
	RemoteTarget string `json:"remote_target,omitempty"`
	RemoteName   string `json:"remote_name,omitempty"`
	RemoteKey    string `json:"remote_key,omitempty"`
	RemoteError  string `json:"remote_error,omitempty"`
}

// RestoreResult reports what a restore unpacked.
type RestoreResult struct {
	Files           int    `json:"files"`
	Bytes           int64  `json:"bytes"`
	SafetyCopy      string `json:"safety_copy"`
	RestartRequired bool   `json:"restart_required"`
	Pending         bool   `json:"pending"`
	Legacy          bool   `json:"legacy"`
}

// Store manages backup archives under <dataDir>/backups.
type Store struct {
	mu        sync.Mutex
	opMu      sync.Mutex
	dataDir   string
	backupDir string
	log       *slog.Logger
	index     []Meta
	task      Task
	policy    Policy
	s3        S3Targets
	rename    func(string, string) error // test seam for transaction failures
}

// NewStore prepares the backup directory and reconciles the archive index with
// what is actually on disk. An empty dataDir yields a store whose operations
// all return ErrDisabled, which keeps the caller free of nil checks.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{dataDir: dataDir, log: log, rename: os.Rename}
	if dataDir == "" {
		return s, nil
	}
	s.backupDir = filepath.Join(dataDir, backupsDirName)
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return nil, fmt.Errorf("create backup dir: %w", err)
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	if err := s.loadOperations(); err != nil {
		return nil, err
	}
	return s, nil
}

// BackupDir exposes where archives are kept, for startup logging.
func (s *Store) BackupDir() string { return s.backupDir }

func (s *Store) indexPath() string { return filepath.Join(s.backupDir, indexFileName) }

// load reads the index and reconciles it against the directory: entries whose
// archive was deleted by hand drop out, and archives copied in by hand are
// adopted with whatever metadata the filesystem can supply.
func (s *Store) load() error {
	data, err := os.ReadFile(s.indexPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read backup index: %w", err)
	}
	var idx []Meta
	if len(data) > 0 {
		if err := json.Unmarshal(data, &idx); err != nil {
			s.log.Warn("backup index unreadable, rebuilding from disk", "err", err)
			idx = nil
		}
	}

	known := make(map[string]bool, len(idx))
	kept := make([]Meta, 0, len(idx))
	for _, m := range idx {
		if !validArchiveName(m.Name) {
			continue
		}
		st, err := os.Stat(filepath.Join(s.backupDir, m.Name))
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		m.Size = st.Size()
		known[m.Name] = true
		kept = append(kept, m)
	}

	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return fmt.Errorf("scan backup dir: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !validArchiveName(e.Name()) || known[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		kept = append(kept, Meta{
			Name:      e.Name(),
			Size:      info.Size(),
			CreatedAt: info.ModTime(),
			Note:      "外部导入（索引中无记录）",
		})
	}

	s.index = kept
	return nil
}

func (s *Store) save() error {
	if s.dataDir == "" {
		return nil
	}
	return atomicJSON(s.indexPath(), s.index)
}

// List returns archive metadata, newest first.
func (s *Store) List() []Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Meta, len(s.index))
	copy(out, s.index)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Path resolves an archive name to a filesystem path, rejecting anything that
// is not a plain archive name in the backup directory so a download or delete
// cannot be steered elsewhere.
func (s *Store) Path(name string) (string, error) {
	if s.dataDir == "" {
		return "", ErrDisabled
	}
	if !validArchiveName(name) {
		return "", errors.New("invalid backup name")
	}
	p := filepath.Join(s.backupDir, name)
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
		return "", errors.New("backup not found")
	}
	return p, nil
}

func validArchiveName(name string) bool {
	return name != "" && name == filepath.Base(name) && !strings.ContainsAny(name, `/\`) && strings.HasPrefix(name, archivePrefix) && strings.HasSuffix(name, archiveSuffix)
}

// Delete removes an archive and its index entry. The returned warning is set
// when the archive's S3 copy could not be cleaned but local deletion
// proceeded anyway, because the recorded target is gone and retrying would
// never succeed.
func (s *Store) Delete(name string) (string, error) {
	if !s.opMu.TryLock() {
		return "", ErrBusy
	}
	defer s.opMu.Unlock()
	return s.delete(name)
}

func (s *Store) delete(name string) (string, error) {
	warning, err := s.deleteRemote(name)
	if err != nil {
		return warning, err
	}
	p, err := s.Path(name)
	if err != nil {
		return warning, err
	}
	if err := os.Remove(p); err != nil {
		return warning, fmt.Errorf("remove backup: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.index {
		if m.Name == name {
			s.index = append(s.index[:i], s.index[i+1:]...)
			break
		}
	}
	return warning, s.save()
}

// Create archives the whole data directory (minus the backup directory itself)
// into a new gzipped tar and records it in the index.
func (s *Store) Create(createdBy, note string, auto bool) (Meta, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.hasPending() {
		return Meta{}, ErrPending
	}
	return s.create(createdBy, note, kindOf(auto))
}

func kindOf(auto bool) string {
	if auto {
		return "safety"
	}
	return "manual"
}

// create persists Kind in the index up front, so the archive is never recorded
// under the wrong retention class even if the process dies before the task
// goroutine finishes remote replication.
func (s *Store) create(createdBy, note, kind string) (Meta, error) {
	if s.dataDir == "" {
		return Meta{}, ErrDisabled
	}

	now := time.Now()
	name, dest := s.freeArchiveName(now)

	// Writing to a temp name first means a crash mid-archive cannot leave a
	// truncated file that looks like a usable backup.
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Meta{}, fmt.Errorf("create archive: %w", err)
	}

	hasher := sha256.New()
	gz := gzip.NewWriter(&limitedWriter{w: io.MultiWriter(f, hasher), remaining: MaxArchiveBytes})
	tw := tar.NewWriter(gz)

	files, walkErr := s.writeTree(tw)

	cerr := tw.Close()
	gerr := gz.Close()
	ferr := f.Close()
	if err := firstErr(walkErr, cerr, gerr, ferr); err != nil {
		os.Remove(tmp)
		return Meta{}, err
	}

	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return Meta{}, fmt.Errorf("finalize archive: %w", err)
	}

	st, err := os.Stat(dest)
	if err != nil {
		return Meta{}, fmt.Errorf("stat archive: %w", err)
	}

	meta := Meta{
		Name:      name,
		Size:      st.Size(),
		Files:     files,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
		Note:      note,
		CreatedBy: createdBy,
		CreatedAt: now,
		Auto:      kind != "manual",
		Kind:      kind,
	}

	s.mu.Lock()
	s.index = append(s.index, meta)
	saveErr := s.save()
	s.mu.Unlock()
	if saveErr != nil {
		// The archive is on disk and load() would adopt it on the next start,
		// so a failed index write is worth reporting but not fatal.
		s.log.Warn("backup index write failed", "name", name, "err", saveErr)
	}

	s.log.Info("backup created", "name", name, "files", files, "bytes", meta.Size, "by", createdBy)
	return meta, nil
}

// freeArchiveName picks a name that is not taken yet. Names are second
// resolution, and two backups can legitimately land in the same second -- the
// pre-restore safety copy is written while the archive being restored is open
// for reading, and overwriting that file would fail on Windows and corrupt the
// restore everywhere else.
func (s *Store) freeArchiveName(now time.Time) (string, string) {
	base := archivePrefix + now.Format("20060102-150405.000000000")
	for i := 0; ; i++ {
		name := base + archiveSuffix
		if i > 0 {
			name = fmt.Sprintf("%s-%d%s", base, i+1, archiveSuffix)
		}
		dest := filepath.Join(s.backupDir, name)
		if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(dest + ".part"); errors.Is(err, os.ErrNotExist) {
				return name, dest
			}
		}
	}
}

// writeTree walks the data directory into the tar writer, skipping the backup
// directory (so backups never nest) and transient artifacts.
func (s *Store) writeTree(tw *tar.Writer) (int, error) {
	count := 0
	entries := 0
	var total int64
	manifest := archiveManifest{Format: "watchman-control-backup", Version: 1, Files: []archiveFile{}}
	root := filepath.Clean(s.dataDir)

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file removed mid-walk (rotated recording, temp file) must not
			// abort the whole backup.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		if excluded(slashRel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		entries++
		if entries > MaxRestoreEntries {
			return fmt.Errorf("备份条目数超过 %d", MaxRestoreEntries)
		}

		if d.IsDir() {
			if slashRel == backupsDirName {
				return fs.SkipDir
			}
			hdr := &tar.Header{
				Name:     slashRel + "/",
				Mode:     0o700,
				Typeflag: tar.TypeDir,
				ModTime:  time.Now(),
			}
			return tw.WriteHeader(hdr)
		}

		if !d.Type().IsRegular() || strings.HasSuffix(slashRel, ".part") {
			return nil
		}

		src, openErr := os.Open(p)
		if openErr != nil {
			if errors.Is(openErr, os.ErrNotExist) {
				return nil
			}
			return openErr
		}
		defer src.Close()
		info, infoErr := src.Stat()
		if infoErr != nil {
			return infoErr
		}
		total += info.Size()
		if total > MaxRestoreBytes {
			return fmt.Errorf("备份数据超过 64 GiB 恢复上限")
		}

		hdr := &tar.Header{
			Name:     slashRel,
			Mode:     0o600,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		// Copy exactly Size bytes: a file being appended to during the walk
		// would otherwise desynchronise the tar stream.
		h := sha256.New()
		if _, err := io.CopyN(io.MultiWriter(tw, h), src, info.Size()); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, archiveFile{Path: slashRel, Size: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil))})
		count++
		s.progress("archiving", count, total)
		return nil
	})

	if err != nil {
		return count, err
	}
	return count, writeManifest(tw, manifest)
}

func excluded(name string) bool {
	name = strings.ToLower(name)
	first := strings.SplitN(name, "/", 2)[0]
	return first == backupsDirName || first == "bin" || first == "volume-uploads" || name == manifestName || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp") || strings.HasPrefix(filepath.Base(name), ".write-")
}

type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("备份归档超过 65 GiB 上限")
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// RestoreFile validates and stages an existing archive; startup applies it.
func (s *Store) RestoreFile(name, actor string) (RestoreResult, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.prepareFile(name, actor, "")
}

// Restore never changes live business files. ApplyPending runs before stores open.
func (s *Store) Restore(r io.Reader, actor, note string) (RestoreResult, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.prepare(r, actor, note, "", "")
}

// safeJoin resolves an archive entry name inside root. Entries that are
// absolute, carry a drive letter, or contain any ".." segment are rejected
// outright rather than sanitized: our own archives never produce such names, so
// one means the archive is malformed or hostile, and quietly remapping it would
// let its author choose where files land inside the data directory (tar slip).
func safeJoin(root, name string) (string, error) {
	slash := filepath.ToSlash(name)
	if slash == "" {
		return "", fmt.Errorf("invalid archive entry %q", name)
	}
	if strings.HasPrefix(slash, "/") || strings.Contains(slash, ":") {
		return "", fmt.Errorf("refusing absolute archive entry %q", name)
	}
	for _, seg := range strings.Split(slash, "/") {
		if seg == ".." {
			return "", fmt.Errorf("refusing archive entry outside data dir: %q", name)
		}
	}
	clean := path.Clean(slash)
	if clean == "." || clean == "" {
		return "", fmt.Errorf("invalid archive entry %q", name)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing archive entry outside data dir: %q", name)
	}
	return target, nil
}

// writeFile writes exactly size bytes, refusing an entry whose declared size
// does not match its payload rather than leaving a half-written file in place
// of good data.
func writeFile(target string, r io.Reader, size int64) (int64, error) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", filepath.Base(target), err)
	}
	n, err := io.CopyN(f, r, size)
	closeErr := f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("write %s: %w", filepath.Base(target), err)
	}
	if closeErr != nil {
		return n, fmt.Errorf("write %s: %w", filepath.Base(target), closeErr)
	}
	if n != size {
		return n, fmt.Errorf("truncated archive entry %s: expected %d bytes, got %d", filepath.Base(target), size, n)
	}
	return n, nil
}
