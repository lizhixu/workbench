// Package backup implements control-plane backup and restore (AGENTS.md 5.1
// 数据自主可控, B.8.2 system/backup + system/restore).
//
// A backup is a gzipped tar of the data directory: user accounts, host
// registry, terminal recordings, metrics history, audit trail and settings.
// Restore unpacks an archive back over that directory. Because the running
// process keeps these files in memory and rewrites them on its next save, a
// restore only becomes authoritative after the server is restarted -- callers
// are told so explicitly, and the current state is always archived first so a
// mistaken restore is recoverable.
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
	archivePrefix = "watchman-backup-"
	archiveSuffix = ".tar.gz"
	backupsDirName = "backups"
	indexFileName  = "index.json"

	// Caps on what a restore will unpack, so a malformed or hostile archive
	// cannot exhaust the disk or the inode table.
	MaxRestoreBytes   int64 = 4 << 30
	MaxRestoreEntries       = 200000
)

// ErrDisabled is returned when the store has no data directory to work with.
var ErrDisabled = errors.New("backup disabled: no data directory configured")

// Meta describes one archive on disk.
type Meta struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Files     int       `json:"files"`
	SHA256    string    `json:"sha256"`
	Note      string    `json:"note"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// Auto marks archives the server took on its own (currently the safety
	// copy written just before a restore).
	Auto bool `json:"auto"`
}

// RestoreResult reports what a restore unpacked.
type RestoreResult struct {
	Files           int    `json:"files"`
	Bytes           int64  `json:"bytes"`
	SafetyCopy      string `json:"safety_copy"`
	RestartRequired bool   `json:"restart_required"`
}

// Store manages backup archives under <dataDir>/backups.
type Store struct {
	mu        sync.Mutex
	dataDir   string
	backupDir string
	log       *slog.Logger
	index     []Meta
}

// NewStore prepares the backup directory and reconciles the archive index with
// what is actually on disk. An empty dataDir yields a store whose operations
// all return ErrDisabled, which keeps the caller free of nil checks.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{dataDir: dataDir, log: log}
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
		st, err := os.Stat(filepath.Join(s.backupDir, m.Name))
		if err != nil {
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), archiveSuffix) || known[e.Name()] {
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
	data, err := json.MarshalIndent(s.index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath(), data, 0o600)
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
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", errors.New("invalid backup name")
	}
	if !strings.HasPrefix(name, archivePrefix) || !strings.HasSuffix(name, archiveSuffix) {
		return "", errors.New("invalid backup name")
	}
	p := filepath.Join(s.backupDir, name)
	if _, err := os.Stat(p); err != nil {
		return "", errors.New("backup not found")
	}
	return p, nil
}

// Delete removes an archive and its index entry.
func (s *Store) Delete(name string) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("remove backup: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.index {
		if m.Name == name {
			s.index = append(s.index[:i], s.index[i+1:]...)
			break
		}
	}
	return s.save()
}

// Create archives the whole data directory (minus the backup directory itself)
// into a new gzipped tar and records it in the index.
func (s *Store) Create(createdBy, note string, auto bool) (Meta, error) {
	if s.dataDir == "" {
		return Meta{}, ErrDisabled
	}

	now := time.Now()
	name, dest := s.freeArchiveName(now)

	// Writing to a temp name first means a crash mid-archive cannot leave a
	// truncated file that looks like a usable backup.
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return Meta{}, fmt.Errorf("create archive: %w", err)
	}

	hasher := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, hasher))
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
		Auto:      auto,
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
	base := archivePrefix + now.Format("20060102-150405")
	for i := 0; ; i++ {
		name := base + archiveSuffix
		if i > 0 {
			name = fmt.Sprintf("%s-%d%s", base, i+1, archiveSuffix)
		}
		dest := filepath.Join(s.backupDir, name)
		if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
			return name, dest
		}
	}
}

// writeTree walks the data directory into the tar writer, skipping the backup
// directory (so backups never nest) and transient artifacts.
func (s *Store) writeTree(tw *tar.Writer) (int, error) {
	count := 0
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

		info, infoErr := d.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				return nil
			}
			return infoErr
		}

		src, openErr := os.Open(p)
		if openErr != nil {
			if errors.Is(openErr, os.ErrNotExist) {
				return nil
			}
			return openErr
		}
		defer src.Close()

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
		if _, err := io.CopyN(tw, src, info.Size()); err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
		}
		count++
		return nil
	})

	return count, err
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// RestoreFile restores from an archive already in the backup directory.
func (s *Store) RestoreFile(name, actor string) (RestoreResult, error) {
	p, err := s.Path(name)
	if err != nil {
		return RestoreResult{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open backup: %w", err)
	}
	defer f.Close()
	return s.Restore(f, actor, "restore-from:"+name)
}

// Restore unpacks a gzipped tar over the data directory. The pre-restore state
// is archived first, so an unintended restore can be walked back.
//
// Files are written but nothing already on disk is deleted: a restore overlays
// the archive. That is deliberate -- a partial archive should not silently
// erase host registrations it never contained.
func (s *Store) Restore(r io.Reader, actor, note string) (RestoreResult, error) {
	if s.dataDir == "" {
		return RestoreResult{}, ErrDisabled
	}

	safety, err := s.Create(actor, "restore 前自动备份 ("+note+")", true)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("pre-restore backup failed, aborting: %w", err)
	}

	gz, err := gzip.NewReader(r)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()

	root, err := filepath.Abs(s.dataDir)
	if err != nil {
		return RestoreResult{}, err
	}

	tr := tar.NewReader(gz)
	res := RestoreResult{SafetyCopy: safety.Name, RestartRequired: true}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res, fmt.Errorf("read archive: %w", err)
		}
		if res.Files >= MaxRestoreEntries {
			return res, fmt.Errorf("archive has more than %d entries, refusing", MaxRestoreEntries)
		}

		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return res, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return res, fmt.Errorf("mkdir %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if res.Bytes+hdr.Size > MaxRestoreBytes {
				return res, fmt.Errorf("archive exceeds the %d byte restore limit", MaxRestoreBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return res, fmt.Errorf("mkdir %s: %w", filepath.Dir(hdr.Name), err)
			}
			n, err := writeFile(target, tr, hdr.Size)
			if err != nil {
				return res, err
			}
			res.Bytes += n
			res.Files++
		default:
			// Symlinks, devices and hard links have no place in a data
			// directory backup and are the usual escape hatch out of it.
			s.log.Warn("backup restore skipped non-regular entry", "name", hdr.Name, "type", hdr.Typeflag)
		}
	}

	s.mu.Lock()
	reloadErr := s.load()
	s.mu.Unlock()
	if reloadErr != nil {
		s.log.Warn("backup index reload after restore failed", "err", reloadErr)
	}

	s.log.Info("backup restored", "files", res.Files, "bytes", res.Bytes, "by", actor, "safety_copy", safety.Name)
	return res, nil
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
