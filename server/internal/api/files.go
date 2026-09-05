package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/audit"
	"watchman/server/internal/rpc"

	"github.com/gin-gonic/gin"
)

// ---- File operations --------------------------------------------------
//
// All file ops use the FileOp proto message and get responses via FileChunk
// (for read/list/stat) or Ack (for write/mkdir/move/remove/copy).

func (h *handlers) fileList(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}
	h.fileOpWithChunkResponse(c, &agentpb.FileOp{OpId: randomToken(8), Op: "list", Path: path})
}

func (h *handlers) fileStat(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}
	h.fileOpWithChunkResponse(c, &agentpb.FileOp{OpId: randomToken(8), Op: "stat", Path: path})
}

func (h *handlers) fileMkdir(c *gin.Context) {
	var body struct {
		Path string `json:"path"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.fileOpWithAck(c, &agentpb.FileOp{OpId: randomToken(8), Op: "mkdir", Path: body.Path})
}

func (h *handlers) fileMove(c *gin.Context) {
	var body struct {
		Path     string `json:"path"`
		DestPath string `json:"dest_path"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.fileOpWithAck(c, &agentpb.FileOp{OpId: randomToken(8), Op: "move", Path: body.Path, DestPath: body.DestPath})
}

func (h *handlers) fileCopy(c *gin.Context) {
	var body struct {
		Path     string `json:"path"`
		DestPath string `json:"dest_path"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.fileOpWithAck(c, &agentpb.FileOp{OpId: randomToken(8), Op: "copy", Path: body.Path, DestPath: body.DestPath})
}

func (h *handlers) fileRemove(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}
	h.fileOpWithAck(c, &agentpb.FileOp{OpId: randomToken(8), Op: "remove", Path: path})
}

// fileDownload streams file content back as binary.
func (h *handlers) fileDownload(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}

	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	h.recordAudit(c, "file_download", "host", c.Param("id"), "下载文件: "+path, audit.RiskLow, audit.ResultSuccess)

	opID := randomToken(8)
	chunkCh := make(chan *agentpb.FileChunk, 64)
	done := make(chan struct{})
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		if msg == nil {
			close(done)
			return
		}
		if ack := msg.GetAck(); ack != nil && !ack.GetOk() {
			close(done)
			return
		}
		fc := msg.GetFileChunk()
		if fc == nil {
			return
		}
		chunkCh <- fc
		if fc.GetEof() {
			close(done)
		}
	})
	defer hub.SetRespHandler(opID, nil)

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_FileOp{
			FileOp: &agentpb.FileOp{OpId: opID, Op: "read", Path: path},
		},
	})

	// Stream chunks to the HTTP response.
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", baseName(path)))
	c.Status(http.StatusOK)

	timeout := time.After(60 * time.Second)
	for {
		select {
		case fc := <-chunkCh:
			if fc.GetEof() {
				return
			}
			if len(fc.GetData()) > 0 {
				_, _ = c.Writer.Write(fc.GetData())
				c.Writer.Flush()
			}
		case <-done:
			return
		case <-timeout:
			return
		}
	}
}

// fileUpload receives a file body and writes it to the agent in chunks.
// MVP: reads the full body into memory (capped at 100MB).
func (h *handlers) fileUpload(c *gin.Context) {
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path required"})
		return
	}

	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	// Read the uploaded file from the request body.
	body := c.Request.Body
	defer body.Close()

	const maxUpload = 100 * 1024 * 1024 // 100MB
	data, err := readAll(body, maxUpload)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	opID := randomToken(8)

	// The agent verifies the finished file against this digest and discards a
	// corrupt result rather than leaving a half-written file on the host.
	sum := sha256.Sum256(data)
	expected := hex.EncodeToString(sum[:])

	// Register ack handler.
	ackCh := make(chan *agentpb.Ack, 1)
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		if msg == nil {
			ackCh <- nil
			return
		}
		ackCh <- msg.GetAck()
	})

	// Send initial write op (overwrite=true) with total_size.
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_FileOp{
			FileOp: &agentpb.FileOp{
				OpId:      opID,
				Op:        "write",
				Path:      path,
				Overwrite: true,
				TotalSize: int64(len(data)),
				Sha256:    expected,
			},
		},
	})

	// Send data in 64KB chunks.
	chunkSize := 64 * 1024
	var seq uint32
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]
		hub.Send(&agentpb.ServerMessage{
			Payload: &agentpb.ServerMessage_FileOp{
				FileOp: &agentpb.FileOp{
					OpId:      opID,
					Op:        "write",
					Path:      path,
					ChunkSeq:  seq,
					Data:      chunk,
					Overwrite: true,
					TotalSize: int64(len(data)),
				},
			},
		})
		seq++
	}

	// Wait for ack.
	defer hub.SetRespHandler(opID, nil)
	select {
	case ack := <-ackCh:
		if ack == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		if !ack.GetOk() {
			h.recordAudit(c, "file_upload", "host", c.Param("id"),
				fmt.Sprintf("上传文件失败 (%d 字节): %s (%s)", len(data), path, ack.GetError()),
				audit.RiskLow, audit.ResultFailed)
			c.JSON(http.StatusInternalServerError, gin.H{"error": ack.GetError()})
			return
		}
		if got := ack.GetSha256(); got != "" && got != expected {
			// The agent reported ok but with a different digest — treat it as a
			// failed upload rather than silently accepting corrupt content.
			h.recordAudit(c, "file_upload", "host", c.Param("id"),
				fmt.Sprintf("上传文件校验失败 (%d 字节): %s (期望 %s, 实际 %s)", len(data), path, expected, got),
				audit.RiskLow, audit.ResultFailed)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "sha256 校验失败"})
			return
		}
		h.recordAudit(c, "file_upload", "host", c.Param("id"),
			fmt.Sprintf("上传文件 (%d 字节): %s", len(data), path),
			audit.RiskLow, audit.ResultSuccess)
		c.JSON(http.StatusOK, gin.H{"ok": true, "size": len(data), "sha256": expected})
	case <-time.After(60 * time.Second):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "upload timeout"})
	}
}

func readAll(r interface{ Read([]byte) (int, error) }, max int) ([]byte, error) {
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > max {
				return nil, fmt.Errorf("upload exceeds %d bytes", max)
			}
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

// fileOpWithChunkResponse sends a FileOp and expects a single FileChunk response
// (containing JSON data) — used for list and stat.
func (h *handlers) fileOpWithChunkResponse(c *gin.Context, op *agentpb.FileOp) {
	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	// A chunk carries the result; an Ack carries the agent's own error text (for
	// example "no such file or directory"), which must reach the caller instead
	// of being flattened into a generic transport failure.
	type chunkResult struct {
		data []byte
		err  string
	}
	resultCh := make(chan chunkResult, 1)
	hub.SetRespHandler(op.GetOpId(), func(msg *agentpb.AgentMessage) {
		if msg == nil {
			resultCh <- chunkResult{err: "agent disconnected"}
			return
		}
		if fc := msg.GetFileChunk(); fc != nil {
			resultCh <- chunkResult{data: fc.GetData()}
			return
		}
		if ack := msg.GetAck(); ack != nil && !ack.GetOk() {
			h.log.Warn("file op error from agent", "op", op.GetOp(), "path", op.GetPath(), "err", ack.GetError())
			resultCh <- chunkResult{err: ack.GetError()}
			return
		}
		resultCh <- chunkResult{err: "empty response from agent"}
	})

	if !hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_FileOp{FileOp: op},
	}) {
		hub.SetRespHandler(op.GetOpId(), nil)
		h.log.Warn("file op send failed, hub stream nil or channel full", "op", op.GetOp(), "path", op.GetPath())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent connection unavailable"})
		return
	}
	defer hub.SetRespHandler(op.GetOpId(), nil)

	select {
	case res := <-resultCh:
		if res.data == nil {
			c.JSON(fileErrStatus(res.err), gin.H{"error": res.err})
			return
		}
		var parsed any
		if json.Unmarshal(res.data, &parsed) == nil {
			c.JSON(http.StatusOK, gin.H{"data": parsed})
		} else {
			c.JSON(http.StatusOK, gin.H{"raw": string(res.data)})
		}
	case <-time.After(10 * time.Second):
		h.log.Warn("file op timeout", "op", op.GetOp(), "path", op.GetPath(), "op_id", op.GetOpId())
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "timeout"})
	}
}

// fileErrStatus maps an agent-side file error to an HTTP status so callers can
// tell "this path is wrong" apart from "the agent is unreachable".
func fileErrStatus(msg string) int {
	switch {
	case msg == "" || strings.Contains(msg, "disconnected") || strings.Contains(msg, "empty response"):
		return http.StatusServiceUnavailable
	case strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "cannot find the file") || strings.Contains(msg, "cannot find the path"):
		return http.StatusNotFound
	case strings.Contains(msg, "permission denied") || strings.Contains(msg, "Access is denied"):
		return http.StatusForbidden
	case strings.Contains(msg, "not a directory") || strings.Contains(msg, "is a directory") ||
		strings.Contains(msg, "file exists") || strings.Contains(msg, "directory not empty") ||
		strings.Contains(msg, "unknown op") || strings.Contains(msg, "seq mismatch"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// fileOpWithAck sends a FileOp and expects an Ack response.
func (h *handlers) fileOpWithAck(c *gin.Context, op *agentpb.FileOp) {
	hub := h.reg.Hub(c.Param("id"))
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	resultCh := make(chan *agentpb.Ack, 1)
	hub.SetRespHandler(op.GetOpId(), func(msg *agentpb.AgentMessage) {
		if msg == nil {
			resultCh <- nil
			return
		}
		resultCh <- msg.GetAck()
	})

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_FileOp{FileOp: op},
	})
	defer hub.SetRespHandler(op.GetOpId(), nil)

	select {
	case ack := <-resultCh:
		if ack == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		if !ack.GetOk() {
			h.auditFileOp(c, op, audit.ResultFailed, ack.GetError())
			c.JSON(fileErrStatus(ack.GetError()), gin.H{"error": ack.GetError()})
			return
		}
		h.auditFileOp(c, op, audit.ResultSuccess, "")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	case <-time.After(10 * time.Second):
		h.auditFileOp(c, op, audit.ResultFailed, "timeout")
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "timeout"})
	}
}

// auditFileOp records a mutating file operation to the unified audit trail.
func (h *handlers) auditFileOp(c *gin.Context, op *agentpb.FileOp, result, errMsg string) {
	if h.audit == nil {
		return
	}
	labels := map[string]struct{ action, desc string }{
		"mkdir":  {"file_mkdir", "新建目录"},
		"move":   {"file_move", "移动/重命名"},
		"copy":   {"file_copy", "复制"},
		"remove": {"file_remove", "删除"},
		"write":  {"file_upload", "上传文件"},
	}
	l, ok := labels[op.GetOp()]
	if !ok {
		return
	}
	detail := fmt.Sprintf("%s: %s", l.desc, op.GetPath())
	if d := op.GetDestPath(); d != "" {
		detail += " -> " + d
	}
	if errMsg != "" {
		detail += " (" + errMsg + ")"
	}
	risk := audit.RiskLow
	if op.GetOp() == "remove" {
		risk = audit.RiskHigh
	}
	h.recordAudit(c, l.action, "host", c.Param("id"), detail, risk, result)
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}

// avoid unused imports
var (
	_ = base64.StdEncoding
	_ = rpc.NewRegistry
)
