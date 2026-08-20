package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"watchman/proto/agentpb"
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": ack.GetError()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "size": len(data)})
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

	resultCh := make(chan []byte, 1)
	hub.SetRespHandler(op.GetOpId(), func(msg *agentpb.AgentMessage) {
		if msg == nil {
			resultCh <- nil
			return
		}
		fc := msg.GetFileChunk()
		if fc != nil {
			resultCh <- fc.GetData()
		} else if ack := msg.GetAck(); ack != nil && !ack.GetOk() {
			h.log.Warn("file op error from agent", "op", op.GetOp(), "err", ack.GetError())
			resultCh <- nil
		} else {
			resultCh <- nil
		}
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
	case data := <-resultCh:
		if data == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no response"})
			return
		}
		var parsed any
		if json.Unmarshal(data, &parsed) == nil {
			c.JSON(http.StatusOK, gin.H{"data": parsed})
		} else {
			c.JSON(http.StatusOK, gin.H{"raw": string(data)})
		}
	case <-time.After(10 * time.Second):
		h.log.Warn("file op timeout", "op", op.GetOp(), "path", op.GetPath(), "op_id", op.GetOpId())
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "timeout"})
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": ack.GetError()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	case <-time.After(10 * time.Second):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "timeout"})
	}
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