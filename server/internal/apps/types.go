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
	// image, i.e. the one-click catalog apps) | "compose" (multi-service
	// stack, phase 4).
	SourceType string `json:"source_type"`

	// Git link configuration (SourceType == "git").
	RepoURL      string `json:"repo_url,omitempty"`
	Branch       string `json:"branch,omitempty"`
	AuthVaultID  string `json:"auth_vault_id,omitempty"` // vault credential: git token / deploy key
	AutoDeploy   bool   `json:"auto_deploy"`
	WebhookToken string `json:"webhook_token,omitempty"` // secret path segment for /apps/webhook/:token

	// Build configuration.
	BuildType      string `json:"build_type,omitempty"`      // "dockerfile" | "compose"
	Dockerfile     string `json:"dockerfile,omitempty"`      // relative path, default "Dockerfile"
	BuildContext   string `json:"build_context,omitempty"`   // relative dir, default "."
	BuildTimeout   int32  `json:"build_timeout_sec,omitempty"`
	ComposeContent string `json:"compose_content,omitempty"` // raw compose.yaml for source_type == "raw_compose"

	// Runtime configuration.
	Image          string            `json:"image,omitempty"`    // resolved image for source_type=image / after build
	EnvVars        map[string]string `json:"env_vars,omitempty"` // supports "{{ vault:<id>:<key> }}" references
	Ports          []PortMapping     `json:"ports,omitempty"`
	Volumes        []string          `json:"volumes,omitempty"`         // "host:container" docker -v specs
	HealthcheckURL string            `json:"healthcheck_url,omitempty"` // e.g. "http://127.0.0.1:8080/healthz"
	ContainerName  string            `json:"container_name,omitempty"`

	// Reverse proxy binding (phase 2: local node vs gateway mesh mode).
	Domain        string `json:"domain,omitempty"`
	ProxyMode     string `json:"proxy_mode,omitempty"`     // "local" | "gateway"
	ProxyUpstream string `json:"proxy_upstream,omitempty"` // e.g. "127.0.0.1:8080" or mesh IP

	// Runtime state.
	CurrentCommit string    `json:"current_commit,omitempty"`
	LastDeployAt  time.Time `json:"last_deploy_at,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// PortMapping is one published container port.
type PortMapping struct {
	Host      int `json:"host"`
	Container int `json:"container"`
}

// Deployment statuses.
const (
	DeployQueued    = "queued"
	DeployBuilding  = "building"
	DeployDeploying = "deploying"
	DeploySuccess   = "success"
	DeployFailed    = "failed"
)

// Deployment records one build+rollout attempt of an application.
type Deployment struct {
	ID            string    `json:"id"`
	AppID         string    `json:"app_id"`
	CommitHash    string    `json:"commit_hash,omitempty"`
	CommitMessage string    `json:"commit_message,omitempty"`
	Trigger       string    `json:"trigger"` // "webhook" | "manual" | "rollback"
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
}
