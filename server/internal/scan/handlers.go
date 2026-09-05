package scan

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"

	"github.com/gin-gonic/gin"
)

// Handlers exposes the scan REST API.
type Handlers struct {
	reg   *rpc.Registry
	store *Store
	log   *slog.Logger
}

// NewHandlers creates scan handlers.
func NewHandlers(reg *rpc.Registry, store *Store, log *slog.Logger) *Handlers {
	if log == nil {
		log = slog.Default()
	}
	return &Handlers{reg: reg, store: store, log: log}
}

// Register mounts scan routes. Reads go on rg; triggering a scan runs commands
// on the target host, so it goes on the caller-supplied write group.
func (h *Handlers) Register(rg *gin.RouterGroup, write *gin.RouterGroup) {
	if write == nil {
		write = rg
	}
	write.POST("/hosts/:id/scans", h.triggerScan)
	rg.GET("/scans", h.listScans)
	rg.GET("/scans/:id", h.getScan)
	rg.GET("/hosts/:id/scans/latest", h.getLatestScan)
}

// triggerScan dispatches a ScanRequest to the agent and waits for completion.
func (h *Handlers) triggerScan(c *gin.Context) {
	hostID := c.Param("id")
	var body struct {
		Type    string `json:"type"`
		RuleSet string `json:"rule_set"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		// Allow empty body — default to baseline scan.
		body.Type = "baseline"
	}
	if body.Type == "" {
		body.Type = "baseline"
	}

	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	agent := h.reg.GetAgent(hostID)
	hostname := ""
	if agent != nil {
		hostname = agent.Hostname
	}

	scanID := "scan-" + randomID()
	job := h.store.CreateJob(scanID, hostID, hostname, body.Type)

	// Set up response handler to accumulate scan progress.
	progressCh := make(chan *agentpb.ScanProgress, 8)
	hub.SetRespHandler(scanID, func(msg *agentpb.AgentMessage) {
		if msg == nil {
			progressCh <- nil
			return
		}
		if p := msg.GetScanProgress(); p != nil {
			progressCh <- p
		}
	})
	// Note: don't auto-clear the handler on return — we need it to keep
	// receiving progress until the scan completes, even if the HTTP client
	// disconnected. We'll clear it manually after the final progress arrives.
	cleanupHandler := func() {
		hub.SetRespHandler(scanID, nil)
	}

	// Send the scan request to the agent.
	sent := hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Scan{
			Scan: &agentpb.ScanRequest{
				ScanId:  scanID,
				Type:    body.Type,
				RuleSet: body.RuleSet,
			},
		},
	})
	if !sent {
		h.store.FailJob(scanID, "failed to send scan request to agent")
		cleanupHandler()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent send buffer full"})
		return
	}

	// Wait for completion (or timeout). Progress updates are streamed to store.
	timeout := time.After(5 * time.Minute)
	clientGone := false
	for {
		select {
		case prog := <-progressCh:
			if prog == nil {
				h.store.FailJob(scanID, "agent disconnected during scan")
				cleanupHandler()
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
				return
			}
			h.store.UpdateProgress(scanID, prog.GetProgress(), prog.GetDone(), prog.GetFindingsJson())
			if prog.GetDone() {
				job = h.store.GetJob(scanID)
				cleanupHandler()
				if clientGone {
					// Client already disconnected; persist result and bail silently.
					return
				}
				c.JSON(http.StatusOK, gin.H{
					"id":             job.ID,
					"host_id":        job.HostID,
					"hostname":       job.Hostname,
					"type":           job.Type,
					"status":         job.Status,
					"started_at":     job.StartedAt,
					"finished_at":    job.FinishedAt,
					"findings_count": job.FindingsCount,
					"findings":       json.RawMessage(job.FindingsJSON),
				})
				return
			}
		case <-timeout:
			h.store.FailJob(scanID, "scan timed out after 5 minutes")
			cleanupHandler()
			if !clientGone {
				c.JSON(http.StatusGatewayTimeout, gin.H{"error": "scan timed out"})
			}
			return
		case <-c.Request.Context().Done():
			// Client disconnected; keep listening for results so the store stays accurate.
			clientGone = true
			// Continue draining progressCh until done or timeout.
			for {
				select {
				case prog := <-progressCh:
					if prog == nil {
						h.store.FailJob(scanID, "agent disconnected during scan")
						cleanupHandler()
						return
					}
					h.store.UpdateProgress(scanID, prog.GetProgress(), prog.GetDone(), prog.GetFindingsJson())
					if prog.GetDone() {
						cleanupHandler()
						return
					}
				case <-timeout:
					h.store.FailJob(scanID, "scan timed out after 5 minutes")
					cleanupHandler()
					return
				}
			}
		}
	}
}

// listScans returns all scan jobs with optional filters.
func (h *Handlers) listScans(c *gin.Context) {
	hostID := c.Query("host_id")
	scanType := c.Query("type")
	status := c.Query("status")

	jobs := h.store.ListJobs(hostID, scanType, status)
	// Strip raw findings from list view for smaller payloads.
	type listJob struct {
		ID            string    `json:"id"`
		HostID        string    `json:"host_id"`
		Hostname      string    `json:"hostname"`
		Type          string    `json:"type"`
		Status        string    `json:"status"`
		StartedAt     time.Time `json:"started_at"`
		FinishedAt    time.Time `json:"finished_at,omitempty"`
		FindingsCount int       `json:"findings_count"`
		Progress      float64   `json:"progress"`
	}
	out := make([]listJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, listJob{
			ID:            j.ID,
			HostID:        j.HostID,
			Hostname:      j.Hostname,
			Type:          j.Type,
			Status:        j.Status,
			StartedAt:     j.StartedAt,
			FinishedAt:    j.FinishedAt,
			FindingsCount: j.FindingsCount,
			Progress:      j.Progress,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out, "total": len(out)})
}

// getScan returns a single scan job with full findings.
func (h *Handlers) getScan(c *gin.Context) {
	job := h.store.GetJob(c.Param("id"))
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "scan not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":             job.ID,
		"host_id":        job.HostID,
		"hostname":       job.Hostname,
		"type":           job.Type,
		"status":         job.Status,
		"started_at":     job.StartedAt,
		"finished_at":    job.FinishedAt,
		"findings_count": job.FindingsCount,
		"findings":       json.RawMessage(job.FindingsJSON),
		"error":          job.Error,
	})
}

// getLatestScan returns the most recent scan for a host.
func (h *Handlers) getLatestScan(c *gin.Context) {
	job := h.store.LatestScanForHost(c.Param("id"))
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no scans found for this host"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":             job.ID,
		"host_id":        job.HostID,
		"hostname":       job.Hostname,
		"type":           job.Type,
		"status":         job.Status,
		"started_at":     job.StartedAt,
		"finished_at":    job.FinishedAt,
		"findings_count": job.FindingsCount,
		"findings":       json.RawMessage(job.FindingsJSON),
	})
}

func randomID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}