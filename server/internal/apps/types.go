package apps

import "time"

// Application is a managed, stateful application entity (Dokploy-style
// "link Git to application"). It aggregates the target host, the container
// runtime config, the Git source driving continuous deploys, the reverse
// proxy binding and (later) the backup policy into one lifecycle object.
type Application struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HostID string `json:"host_id"`
	// SourceType: "git" (build from a linked Git repo) | "image" (pre-built
	// image) | "template" (built-in one-click catalog app) | "raw_compose"
	// (multi-service stack via raw YAML).
	SourceType string `json:"source_type"`

	// Template configuration (SourceType == "template").
	TemplateID     string            `json:"template_id,omitempty"`
	TemplateParams map[string]string `json:"template_params,omitempty"`

	// Git link configuration (SourceType == "git").
	RepoURL            string `json:"repo_url,omitempty"`
	Branch             string `json:"branch,omitempty"`
	AuthVaultID        string `json:"auth_vault_id,omitempty"` // vault credential: git token / deploy key
	AutoDeploy         bool   `json:"auto_deploy"`
	WebhookToken       string `json:"webhook_token,omitempty"` // secret path segment for /apps/webhook/:token
	WebhookAutoManaged bool   `json:"webhook_auto_managed,omitempty"`
	GitHubHookID       int64  `json:"github_hook_id,omitempty"`
	WebhookError       string `json:"webhook_error,omitempty"`

	// Build configuration.
	BuildType      string `json:"build_type,omitempty"`    // "dockerfile" | "compose"
	Dockerfile     string `json:"dockerfile,omitempty"`    // relative path, default "Dockerfile"
	BuildContext   string `json:"build_context,omitempty"` // relative dir, default "."
	BuildTimeout   int32  `json:"build_timeout_sec,omitempty"`
	ComposeContent string `json:"compose_content,omitempty"` // raw compose.yaml for source_type == "raw_compose"

	// Runtime configuration.
	Image          string            `json:"image,omitempty"`    // resolved image for source_type=image / after build
	EnvVars        map[string]string `json:"env_vars,omitempty"` // supports "{{ vault:<id>:<key> }}" references
	Ports          []PortMapping     `json:"ports,omitempty"`
	Volumes        []string          `json:"volumes,omitempty"`         // "host:container" docker -v specs
	HealthcheckURL string            `json:"healthcheck_url,omitempty"` // e.g. "http://127.0.0.1:8080/healthz"
	ContainerName  string            `json:"container_name,omitempty"`

	// Mesh Forwarding (TCP Relay via an entry host).
	// When enabled, an alpine/socat relay container runs on MeshForwardHostID,
	// listening on MeshForwardEntryPort and forwarding to MeshForwardTarget
	// (resolved from MeshForwardTargetPort on the app host).
	MeshForwardEnabled    bool   `json:"mesh_forward_enabled"`
	MeshForwardHostID     string `json:"mesh_forward_host_id,omitempty"`     // Entry host ID
	MeshForwardEntryPort  int    `json:"mesh_forward_entry_port,omitempty"`  // Port on entry host
	MeshForwardTargetPort int    `json:"mesh_forward_target_port,omitempty"` // Target port on app host
	MeshForwardTarget     string `json:"mesh_forward_target,omitempty"`      // Resolved host:port target

	// Reverse proxy binding.
	// Domain is a LEGACY field: it only exists so json.Unmarshal can capture
	// old apps.json records; loadApps migrates it into ProxyDomains (with an
	// empty CertID = legacy entry) and clears it. New code must never read or
	// write it.
	Domain string `json:"domain,omitempty"`
	// ProxyDomains lists every bound hostname with the certificate that
	// serves it — each domain carries its own explicit CertID (no domain
	// auto-matching, no auto-issuance on bind).
	ProxyDomains       []DomainBinding `json:"proxy_domains,omitempty"`
	ProxyMode          string          `json:"proxy_mode,omitempty"`            // "local" | "gateway"
	ProxyGatewayHostID string          `json:"proxy_gateway_host_id,omitempty"` // gateway node host id
	ProxyUpstream      string          `json:"proxy_upstream,omitempty"`        // e.g. "127.0.0.1:8080" or mesh IP
	// ProxyUpstreamManual records that ProxyUpstream was supplied by the
	// operator at bind time (req.Upstream non-empty) rather than computed
	// by defaultUpstream. SyncProxyForHost's gateway branch honors it by
	// keeping the stored value verbatim across migrations; without it the
	// old-host heuristic could recompute away a manual value that happens
	// to collide with the old host + first published port, or preserve a
	// computed value whose host IP has since drifted. Legacy records
	// (false) keep the heuristic. Cleared on unbind with the rest of the
	// binding.
	ProxyUpstreamManual bool `json:"proxy_upstream_manual,omitempty"`
	// ProxyWebSocket persists the bind-time WebSocket choice (Upgrade /
	// Connection headers + 300s read timeout). It must survive migrations:
	// SyncProxyForHost used to hardcode websocket=true, silently flipping
	// apps whose websocket support was off. It is a pointer on purpose:
	// nil means "no choice recorded" (legacy records, and binds from
	// callers that do not send the field, e.g. the create page), which
	// renders exactly like the pre-change behavior — Upgrade headers on.
	// Only an explicit false turns them off. A plain bool could not tell
	// "legacy, always-on" apart from "explicitly off", and migrations
	// would have silently dropped Upgrade headers from legacy apps.
	ProxyWebSocket *bool `json:"proxy_websocket,omitempty"`

	// Runtime state.
	CurrentCommit string    `json:"current_commit,omitempty"`
	LastDeployAt  time.Time `json:"last_deploy_at,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// UpdatedAt advances on every persisted mutation (config edit, deploy
	// result, proxy bind/unbind, mesh forward sync). Store.UpdateApp is the
	// single write point, so no call site can forget it.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// PortMapping is one published container port.
type PortMapping struct {
	Host      int `json:"host"`
	Container int `json:"container"`
	// BindScope: "public" (binds to 0.0.0.0) | "mesh" (binds to Tailscale IP).
	// Empty string defaults to "public".
	BindScope string `json:"bind_scope,omitempty"`
}

// DomainBinding is one explicitly bound hostname plus the certificate that
// serves it. CertID is mandatory on every new bind (the API validates the
// certificate exists AND covers the domain); the empty CertID only occurs on
// entries migrated from the legacy single Domain field, which are resolved
// via deterministic best-match and frozen back on first use.
type DomainBinding struct {
	Domain string `json:"domain"`
	CertID string `json:"cert_id"`
}

// Deployment statuses.
const (
	DeployQueued    = "queued"
	DeployBuilding  = "building"
	DeployDeploying = "deploying"
	DeploySuccess   = "success"
	DeployFailed    = "failed"
	// DeployCancelled marks a run aborted by an operator via the cancel
	// endpoint: distinct from DeployFailed (a pipeline error) so the UI and
	// alerts can tell "the user stopped it" from "the build broke".
	DeployCancelled = "cancelled"
)

// Deployment records one build+rollout attempt of an application.
type Deployment struct {
	ID            string    `json:"id"`
	AppID         string    `json:"app_id"`
	CommitHash    string    `json:"commit_hash,omitempty"`
	CommitMessage string    `json:"commit_message,omitempty"`
	Trigger       string    `json:"trigger"` // "create" | "webhook" | "manual" | "rollback" | "migrate"
	Status        string    `json:"status"`
	StartedBy     string    `json:"started_by,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
	DurationMS    int64     `json:"duration_ms,omitempty"`
	ExitCode      int32     `json:"exit_code,omitempty"`
	Error         string    `json:"error,omitempty"`
	// BuildLog holds the captured stdout/stderr of the build+rollout steps,
	// trimmed to a sane bound so the JSONL history stays readable.
	BuildLog string `json:"build_log,omitempty"`
	// ComposeContent snapshots the raw compose.yaml for source_type ==
	// "raw_compose" deployments, so rollback can restore exactly this
	// version. Older records predate the snapshot and cannot be rolled back.
	ComposeContent string `json:"compose_content,omitempty"`
	// CancelledBy records the username that aborted this run (only set when
	// Status == DeployCancelled).
	CancelledBy string `json:"cancelled_by,omitempty"`
}

// MigrateAppReq is the payload of POST /apps/:id/migrate.
type MigrateAppReq struct {
	TargetHostID   string `json:"target_host_id"`
	MigrateVolumes bool   `json:"migrate_volumes"`
	CleanSource    bool   `json:"clean_source"`
}

// MigratePrecheckResp reports target-host readiness for a migration. It is
// advisory: problems surface as warnings so the operator can still decide.
type MigratePrecheckResp struct {
	TargetHostID  string   `json:"target_host_id"`
	AgentOnline   bool     `json:"agent_online"`
	DockerRunning bool     `json:"docker_running"`
	VolumeCount   int      `json:"volume_count"`
	S3Ready       bool     `json:"s3_ready"`
	S3Bucket      string   `json:"s3_bucket,omitempty"`
	PortConflicts []int    `json:"port_conflicts,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}
