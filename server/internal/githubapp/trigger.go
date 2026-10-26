package githubapp

// TriggerResult is one app's outcome for a push delivery.
type TriggerResult struct {
	AppID        string `json:"app_id"`
	AppName      string `json:"app_name"`
	DeploymentID string `json:"deployment_id,omitempty"`
	Skipped      string `json:"skipped,omitempty"`
}

// PushTrigger turns a push delivery into deployments. It is implemented by
// the apps package, which owns the deployment engine; this package only
// owns the GitHub App protocol side.
type PushTrigger interface {
	TriggerPush(repoFullName, branch string) []TriggerResult
}
