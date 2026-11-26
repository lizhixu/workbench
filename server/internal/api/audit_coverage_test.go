package api

import (
	"net/http"
	"strings"
	"testing"

	"watchman/server/internal/audit"
)

// TestDeriveMutationActionCoverage pins the audit coverage of every mutating
// route: adding a write endpoint without a rule here is a review failure.
func TestDeriveMutationActionCoverage(t *testing.T) {
	cases := []struct {
		method, pattern string
		action          string
		targetType      string
		risk            string // empty = medium
	}{
		{http.MethodPost, "/api/v1/auth/logout", "logout", "system", audit.RiskLow},
		// Users.
		{http.MethodPost, "/api/v1/users", "user_create", "", ""},
		{http.MethodDelete, "/api/v1/users/:username", "user_delete", "", audit.RiskHigh},
		// Apps.
		{http.MethodPost, "/api/v1/apps", "app_create", "app", ""},
		{http.MethodPost, "/api/v1/apps/:id/deploy", "app_deploy", "app", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/upgrade", "app_upgrade", "app", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/deployments/cancel", "app_deploy_cancel", "app", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/migrate", "app_migrate", "app", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/volume-backups", "app_volume_backup", "app", ""},
		{http.MethodPost, "/api/v1/apps/:id/volume-backups/:archiveID/restore", "app_volume_restore", "app", audit.RiskHigh},
		{http.MethodPost, "/api/v1/apps/:id/volume-backups/upload-scan", "app_volume_upload_scan", "app", audit.RiskMedium},
		{http.MethodPost, "/api/v1/apps/:id/volume-backups/upload-commit", "app_volume_upload_restore", "app", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/apps/:id/volume-backups/:archiveID", "app_volume_backup_delete", "app", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/apps/:id/proxy", "app_proxy_unbind", "app", audit.RiskHigh},
		// Cert hub.
		{http.MethodPost, "/api/v1/certs/issue", "cert_issue", "cert", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/manual/:id/confirm", "cert_manual_dns_confirm", "cert", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/certs/manual/:id", "cert_manual_dns_cancel", "cert", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/import", "cert_import", "cert", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/:id/renew", "cert_renew", "cert", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/:id/reissue", "cert_reissue", "cert", audit.RiskHigh},
		{http.MethodPut, "/api/v1/certs/:id/remark", "cert_remark", "cert", ""},
		{http.MethodDelete, "/api/v1/certs/:id", "cert_delete", "cert", audit.RiskHigh},
		// File ops.
		{http.MethodPost, "/api/v1/hosts/:id/files/mkdir", "file_mkdir", "host", ""},
		{http.MethodPost, "/api/v1/hosts/:id/files/chmod", "file_chmod", "host", ""},
		{http.MethodPost, "/api/v1/hosts/:id/files/compress", "file_compress", "host", ""},
		{http.MethodDelete, "/api/v1/hosts/:id/files", "file_remove", "host", audit.RiskHigh},
		{http.MethodPost, "/api/v1/hosts/:id/scans", "scan_trigger", "host", ""},
		{http.MethodPost, "/api/v1/hosts/:id/traffic/correct", "traffic_correct", "host", ""},
		// Terminal.
		{http.MethodDelete, "/api/v1/terminals/:sid", "terminal_close", "session", audit.RiskHigh},
		// Backups.
		{http.MethodPost, "/api/v1/system/backup", "system_backup", "", audit.RiskMedium},
		{http.MethodDelete, "/api/v1/system/backups/:name", "system_backup_delete", "", audit.RiskHigh},
		{http.MethodPost, "/api/v1/system/backups/:name/restore", "system_restore", "", audit.RiskHigh},
		{http.MethodPost, "/api/v1/system/restore", "system_restore", "", audit.RiskHigh},
		{http.MethodPut, "/api/v1/system/backups/policy", "system_backup_policy", "system", audit.RiskMedium},
		{http.MethodDelete, "/api/v1/system/restore/pending", "system_restore_cancel", "system", audit.RiskHigh},
		{http.MethodPost, "/api/v1/backups/jobs/:id/archives/:archiveID/restore", "backup_archive_restore", "backup", audit.RiskHigh},
		{http.MethodPost, "/api/v1/backups/s3-targets/:id/test", "backup_s3_test", "backup", audit.RiskLow},
		// GitHub App.
		{http.MethodPost, "/api/v1/github-app/manifest", "github_app_manifest", "system", audit.RiskLow},
		{http.MethodPost, "/api/v1/github-app/manifest/exchange", "github_app_config", "system", audit.RiskHigh},
		{http.MethodPost, "/api/v1/github-app/config", "github_app_config", "system", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/github-app", "github_app_disconnect", "system", audit.RiskHigh},
		{http.MethodPost, "/api/v1/github-app/installations/sync", "github_app_install_sync", "system", ""},
		// Groups.
		{http.MethodPost, "/api/v1/groups/:id/users", "group_grant", "group", audit.RiskHigh},
		// Commands.
		{http.MethodDelete, "/api/v1/commands/:id", "command_delete", "command", audit.RiskHigh},
		// Alerts.
		{http.MethodDelete, "/api/v1/alerts/rules/:id", "alert_rule_delete", "alert", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/alerts/events", "alert_events_clear", "alert", audit.RiskHigh},
		{http.MethodPut, "/api/v1/alerts/webhook", "alert_webhook_update", "alert", ""},
		// AI config.
		{http.MethodPut, "/api/v1/ai/config", "ai_config_update", "system", ""},
		// Vault family.
		{http.MethodPost, "/api/v1/vault/credentials", "vault_op", "", audit.RiskHigh},
	}
	for _, tc := range cases {
		rule, ok := deriveMutationAction(tc.method, tc.pattern)
		if !ok {
			t.Errorf("%s %s: no audit rule", tc.method, tc.pattern)
			continue
		}
		if rule.action != tc.action {
			t.Errorf("%s %s: action = %q, want %q", tc.method, tc.pattern, rule.action, tc.action)
		}
		if rule.targetType != tc.targetType {
			t.Errorf("%s %s: targetType = %q, want %q", tc.method, tc.pattern, rule.targetType, tc.targetType)
		}
		wantRisk := tc.risk
		if wantRisk == "" {
			wantRisk = audit.RiskMedium
		}
		gotRisk := rule.risk
		if gotRisk == "" {
			gotRisk = audit.RiskMedium
		}
		if gotRisk != wantRisk {
			t.Errorf("%s %s: risk = %q, want %q", tc.method, tc.pattern, gotRisk, wantRisk)
		}
		// The detail column of the audit trail must read as a Chinese
		// operation description, never as an "METHOD /route" dump.
		if strings.TrimSpace(rule.desc) == "" {
			t.Errorf("%s %s: rule has no desc, audit detail would fall back to the raw route", tc.method, tc.pattern)
		}
	}

	// Every rule in the table must carry a desc, not just the sampled ones.
	for _, r := range deriveAllMutationRules() {
		if strings.TrimSpace(r.desc) == "" {
			t.Errorf("%s %s: rule has no desc", r.method, r.pattern)
		}
	}

	// Read-only and diagnostic routes must stay silent.
	silent := [][2]string{
		{http.MethodGet, "/api/v1/apps"},
		{http.MethodPost, "/api/v1/ai/diagnose"},
		{http.MethodPost, "/api/v1/ai/chat"},
		{http.MethodPost, "/api/v1/ai/models"},
		{http.MethodPost, "/api/v1/apps/webhook/:token"},
		{http.MethodPost, "/api/v1/apps/:id/migrate/precheck"},
	}
	for _, s := range silent {
		if _, ok := deriveMutationAction(s[0], s[1]); ok {
			t.Errorf("%s %s: should not be audited", s[0], s[1])
		}
	}
}

// TestExplicitAuditRoutesExcludedFromMutationTable pins the routes that
// audit themselves inside their handlers instead of through the mutation
// middleware: firewall allow/deny/delete, killProcess and systemCleanup.
// They must stay out of deriveAllMutationRules for two reasons. The
// middleware only records successes (status < 400) with a static desc, while
// these handlers record failures too and put the port/PID in the detail —
// moving them into the table would silently drop failure rows and the
// identifying detail. And if one of these routes were ever wrapped in
// auditMutation, a table entry would double-record every success. The
// explicit recordAudit call in each handler is the audit; this test is the
// guard that keeps the two mechanisms from overlapping.
func TestExplicitAuditRoutesExcludedFromMutationTable(t *testing.T) {
	explicit := [][2]string{
		{http.MethodPost, "/api/v1/hosts/:id/firewall/allow"},
		{http.MethodPost, "/api/v1/hosts/:id/firewall/deny"},
		{http.MethodPost, "/api/v1/hosts/:id/firewall/delete"},
		{http.MethodPost, "/api/v1/hosts/:id/processes/:pid/kill"},
		{http.MethodPost, "/api/v1/hosts/:id/system-cleanup"},
	}
	for _, s := range explicit {
		if rule, ok := deriveMutationAction(s[0], s[1]); ok {
			t.Errorf("%s %s: must stay out of the mutation table (audited explicitly in its handler), got action %q", s[0], s[1], rule.action)
		}
	}
}
