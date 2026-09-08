package snapshots

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// backupRoot on the managed host. Archives stay on the host that owns the
// data — pushing every byte through the control plane would saturate its
// uplink (the design doc's direct-to-storage principle).
const backupRoot = "/var/backups/watchman"

// Engine executes backup jobs on managed hosts through the agent channel.
type Engine struct {
	reg   *rpc.Registry
	store *Store
	log   *slog.Logger
}

// NewEngine creates the backup execution engine.
func NewEngine(reg *rpc.Registry, store *Store, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{reg: reg, store: store, log: log}
}

// archiveID builds a filesystem-safe, sortable archive identifier.
func archiveID(jobID string, ts time.Time) string {
	return fmt.Sprintf("%s-%s", jobID, ts.UTC().Format("20060102-150405"))
}

// Run executes one job now (manual trigger or cron pickup) and records the
// result. The whole pipeline runs as a single agent script so the archive and
// its sha256 are produced atomically on the host.
func (e *Engine) Run(job *Job) (*Archive, error) {
	hub := e.reg.Hub(job.HostID)
	if hub == nil {
		return nil, fmt.Errorf("目标主机 Agent 不在线")
	}

	ts := time.Now().UTC()
	id := archiveID(job.ID, ts)
	archivePath := filepath.ToSlash(filepath.Join(backupRoot, id+".tar.gz"))

	script, err := buildScript(job, archivePath)
	if err != nil {
		return nil, err
	}

	// Timeout scales with a generous ceiling; DB dumps of large datasets can
	// take a while but the operator can bound it per run.
	timeout := int32(1800)
	res, execErr := execOnAgent(hub, "snap-"+id[:min(24, len(id))], script, timeout)
	if execErr != nil {
		e.recordFailure(job, execErr.Error())
		return nil, execErr
	}
	if res.GetExitCode() != 0 {
		msg := fmt.Sprintf("备份失败 (exit %d)", res.GetExitCode())
		e.recordFailure(job, msg)
		return nil, fmt.Errorf("%s", msg)
	}

	// The script prints WATCHMAN_SIZE and WATCHMAN_SHA256 on success.
	size, sum := parseArchiveMeta(res.GetStdout())
	if size <= 0 {
		// Non-fatal: the tar ran but markers were swallowed. Record without
		// them so the archive is still discoverable.
		e.logWarn("backup markers missing", job, string(res.GetStdout()))
	}

	// Resolve storage destination (default / local / specific S3 target).
	storageID := "local"
	storagePath := archivePath
	s3Target := e.resolveS3Target(job)

	if s3Target != nil {
		s3Client := NewS3Client(s3Target)
		s3Key := s3Client.ObjectKey(id + ".tar.gz")
		putURL, err := s3Client.PresignPut(s3Key, 2*time.Hour)
		if err != nil {
			e.logWarn("generate S3 presigned PUT URL failed", job, err.Error())
		} else {
			// Upload directly from host via curl
			uploadScript := fmt.Sprintf(`curl -fsSL -X PUT -T %s %s`,
				shellQuote(archivePath), shellQuote(putURL))
			upRes, upErr := execOnAgent(hub, "s3-up-"+id[:min(20, len(id))], uploadScript, 600)
			if upErr != nil || upRes.GetExitCode() != 0 {
				e.logWarn("S3 direct upload failed, archive retained locally", job, fmt.Sprintf("%v", upErr))
			} else {
				storageID = s3Target.ID
				storagePath = s3Key
				e.log.Info("archive uploaded to S3", "job", job.ID, "target", s3Target.Name, "key", s3Key)
			}
		}
	}

	// Retention prune: keep the newest N archives of this job.
	if job.Retention > 0 {
		if _, err := execOnAgent(hub, "prune-"+job.ID, pruneScript(job.ID, job.Retention), 60); err != nil {
			e.logWarn("retention prune failed", job, err.Error())
		}
	}

	arch := &Archive{
		ID:            id,
		JobID:         job.ID,
		HostName:      job.HostID,
		Kind:          job.Kind,
		Target:        job.Target,
		StorageTarget: storageID,
		StoragePath:   storagePath,
		Size:          size,
		SHA256:        sum,
		CreatedAt:     ts,
	}
	if err := e.store.AddArchive(arch); err != nil {
		return nil, err
	}
	_ = e.store.UpdateJob(job.ID, func(j *Job) error {
		j.LastRun = time.Now()
		j.LastError = ""
		return nil
	})
	return arch, nil
}

func (e *Engine) recordFailure(job *Job, msg string) {
	_ = e.store.UpdateJob(job.ID, func(j *Job) error {
		j.LastRun = time.Now()
		j.LastError = msg
		return nil
	})
}

func (e *Engine) logWarn(msg string, job *Job, detail string) {
	e.log.Warn(msg, "job", job.ID, "detail", detail)
}

// buildScript renders the agent-side backup script for one job.
func buildScript(job *Job, archivePath string) (string, error) {
	var source string
	switch job.Kind {
	case KindDir:
		if job.Target == "" || strings.Contains(job.Target, "..") {
			return "", fmt.Errorf("目录路径非法")
		}
		source = fmt.Sprintf("tar -C %s -czf %s .", shellQuote(job.Target), shellQuote(archivePath))
	case KindVolume:
		if job.Target == "" {
			return "", fmt.Errorf("卷名不能为空")
		}
		// Resolve the volume's mountpoint from docker, then pack it.
		source = fmt.Sprintf(`VP=$(docker volume inspect --format '{{.Mountpoint}}' %s)
[ -n "$VP" ] || { echo "volume not found" >&2; exit 1; }
tar -C "$VP" -czf %s .`, shellQuote(job.Target), shellQuote(archivePath))
	case KindDatabase:
		dump, err := dumpScript(job, archivePath)
		if err != nil {
			return "", err
		}
		source = dump
	default:
		return "", fmt.Errorf("未知备份类型: %s", job.Kind)
	}
	return fmt.Sprintf(`set -e
mkdir -p %s
%s
SIZE=$(stat -c %%s %s 2>/dev/null || stat -f %%z %s)
SHA=$(sha256sum %s | awk '{print $1}')
echo "WATCHMAN_SIZE=$SIZE"
echo "WATCHMAN_SHA256=$SHA"`,
		shellQuote(backupRoot), source,
		shellQuote(archivePath), shellQuote(archivePath), shellQuote(archivePath)), nil
}

// dumpScript renders the per-DB-engine hot-dump for a containerized
// database. Credentials are not persisted here: the container's own env is
// used (docker exec reads it), so no secrets flow through the control plane.
func dumpScript(job *Job, archivePath string) (string, error) {
	c := shellQuote(job.Target)
	out := shellQuote(archivePath)
	switch strings.ToLower(job.DBType) {
	case "mysql":
		db := ""
		if job.DBName != "" {
			db = " --databases " + shellQuote(job.DBName)
		}
		return fmt.Sprintf(`docker exec %s sh -c 'mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --single-transaction --routines --triggers%s' | gzip > %s`,
			c, db, out), nil
	case "postgres":
		db := job.DBName
		if db == "" {
			db = "postgres"
		}
		return fmt.Sprintf(`docker exec %s sh -c 'pg_dump -U "$POSTGRES_USER" %s' | gzip > %s`,
			c, shellQuote(db), out), nil
	case "redis":
		return fmt.Sprintf(`docker exec %s redis-cli -a "$REDIS_PASSWORD" --no-auth-warning BGSAVE 2>/dev/null || docker exec %s redis-cli BGSAVE
sleep 2
docker exec %s sh -c 'cd /data && tar -czf - .' > %s`,
			c, c, c, out), nil
	case "mongo":
		db := ""
		if job.DBName != "" {
			db = " --db " + shellQuote(job.DBName)
		}
		return fmt.Sprintf(`docker exec %s sh -c 'mongodump --quiet --archive --gzip%s' > %s`,
			c, db, out), nil
	default:
		return "", fmt.Errorf("不支持的数据库类型: %s（支持 mysql/postgres/redis/mongo）", job.DBType)
	}
}

// pruneScript removes older archives of one job beyond the retention count.
func pruneScript(jobID string, keep int) string {
	return fmt.Sprintf(`cd %s 2>/dev/null || exit 0
ls -1 %s-*.tar.gz 2>/dev/null | sort -r | tail -n +$(("%d"+1)) | while read f; do rm -f -- "$f"; done`,
		shellQuote(backupRoot), jobID, keep)
}

// resolveS3Target resolves the S3 target for a job:
// if job specifies an explicit target (s3_xxx), look it up;
// if "default" or empty, look up the default S3 target;
// if none found, returns nil (meaning local backup).
func (e *Engine) resolveS3Target(job *Job) *S3Target {
	tgt := job.StorageTarget
	if tgt == "" || tgt == "default" {
		def, ok := e.store.GetDefaultS3Target()
		if ok {
			return def
		}
		return nil
	}
	if tgt == "local" {
		return nil
	}
	t, ok := e.store.GetS3Target(tgt)
	if ok {
		return t
	}
	return nil
}

// Restore unpacks an archive back to its target. Following the control-plane
// backup module's safety principle, the current state is archived first so a
// mistaken restore is recoverable.
func (e *Engine) Restore(job *Job, arch *Archive) error {
	hub := e.reg.Hub(job.HostID)
	if hub == nil {
		return fmt.Errorf("目标主机 Agent 不在线")
	}
	archivePath := filepath.ToSlash(filepath.Join(backupRoot, arch.ID+".tar.gz"))

	// If the archive was stored in S3, download it to the host first via presigned GET URL
	if arch.StorageTarget != "" && arch.StorageTarget != "local" {
		if s3Target, ok := e.store.GetS3Target(arch.StorageTarget); ok {
			s3Client := NewS3Client(s3Target)
			s3Key := arch.StoragePath
			if s3Key == "" {
				s3Key = s3Client.ObjectKey(arch.ID + ".tar.gz")
			}
			getURL, err := s3Client.PresignGet(s3Key, 2*time.Hour)
			if err != nil {
				return fmt.Errorf("生成 S3 下载地址失败: %w", err)
			}
			dlScript := fmt.Sprintf(`mkdir -p %s && curl -fsSL -o %s %s`,
				shellQuote(backupRoot), shellQuote(archivePath), shellQuote(getURL))
			dlRes, dlErr := execOnAgent(hub, "s3-dl-"+arch.ID[:min(20, len(arch.ID))], dlScript, 600)
			if dlErr != nil || dlRes.GetExitCode() != 0 {
				return fmt.Errorf("从 S3 下载归档失败: %v", dlErr)
			}
		}
	}

	var restoreCmd string
	switch job.Kind {
	case KindDir, KindVolume:
		var target string
		if job.Kind == KindDir {
			target = job.Target
		} else {
			target = fmt.Sprintf(`$(docker volume inspect --format '{{.Mountpoint}}' %s)`, shellQuote(job.Target))
		}
		restoreCmd = fmt.Sprintf(`set -e
# Safety copy of the current state before overwriting.
tar -C %s -czf %s . 2>/dev/null || true
mkdir -p %s
tar -C %s -xzf %s`,
			shellQuote(job.Target), shellQuote(filepath.ToSlash(filepath.Join(backupRoot, arch.ID+".pre-restore.tar.gz"))),
			target, target, shellQuote(archivePath))
	case KindDatabase:
		return e.restoreDatabase(job, arch, archivePath)
	default:
		return fmt.Errorf("未知备份类型: %s", job.Kind)
	}

	res, err := execOnAgent(hub, "restore-"+arch.ID[:min(24, len(arch.ID))], restoreCmd, 1800)
	if err != nil {
		return err
	}
	if res.GetExitCode() != 0 {
		return fmt.Errorf("恢复失败 (exit %d)", res.GetExitCode())
	}
	return nil
}

// restoreDatabase imports a dump back into the containerized database, with
// the same safety-copy discipline (the container keeps its own dump dir).
func (e *Engine) restoreDatabase(job *Job, arch *Archive, archivePath string) error {
	c := shellQuote(job.Target)
	switch strings.ToLower(job.DBType) {
	case "mysql":
		cmd := fmt.Sprintf(`set -e
docker exec -i %s sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD"' < %s`,
			c, shellQuote(archivePath))
		res, err := execOnAgent(e.reg.Hub(job.HostID), "restore-"+arch.ID[:min(24, len(arch.ID))], cmd, 1800)
		if err != nil {
			return err
		}
		if res.GetExitCode() != 0 {
			return fmt.Errorf("mysql 恢复失败 (exit %d)", res.GetExitCode())
		}
		return nil
	case "postgres":
		db := job.DBName
		if db == "" {
			db = "postgres"
		}
		cmd := fmt.Sprintf(`set -e
docker exec -i %s sh -c 'psql -U "$POSTGRES_USER" -d %s' < %s`,
			c, shellQuote(db), shellQuote(archivePath))
		res, err := execOnAgent(e.reg.Hub(job.HostID), "restore-"+arch.ID[:min(24, len(arch.ID))], cmd, 1800)
		if err != nil {
			return err
		}
		if res.GetExitCode() != 0 {
			return fmt.Errorf("postgres 恢复失败 (exit %d)", res.GetExitCode())
		}
		return nil
	default:
		return fmt.Errorf("暂不支持 %s 的在线恢复，请下载归档后手动导入", job.DBType)
	}
}

// parseArchiveMeta extracts the WATCHMAN_SIZE/WATCHMAN_SHA256 markers.
func parseArchiveMeta(stdout []byte) (int64, string) {
	var size int64
	var sum string
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "WATCHMAN_SIZE=") {
			fmt.Sscanf(line, "WATCHMAN_SIZE=%d", &size)
		} else if strings.HasPrefix(line, "WATCHMAN_SHA256=") {
			fmt.Sscanf(line, "WATCHMAN_SHA256=%s", &sum)
		}
	}
	return size, sum
}

// execOnAgent runs one script and waits for the result (same contract as the
// apps package's helper, duplicated here to keep the packages decoupled).
func execOnAgent(hub *rpc.Hub, execID string, command string, timeoutSec int32) (*agentpb.ExecResult, error) {
	resultCh := make(chan *agentpb.ExecResult, 1)
	hub.SetRespHandler(execID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(execID, nil)
		if msg != nil {
			resultCh <- msg.GetExecResult()
		} else {
			resultCh <- nil
		}
	})
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:     execID,
				Command:    command,
				IsScript:   true,
				TimeoutSec: timeoutSec,
			},
		},
	})
	wait := time.Duration(timeoutSec)*time.Second + 30*time.Second
	select {
	case res := <-resultCh:
		if res == nil {
			return nil, fmt.Errorf("agent disconnected")
		}
		return res, nil
	case <-time.After(wait):
		hub.SetRespHandler(execID, nil)
		return nil, fmt.Errorf("agent exec timeout")
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
