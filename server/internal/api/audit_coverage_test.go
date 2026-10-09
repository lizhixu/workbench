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
		{http.MethodDelete, "/api/v1/apps/:id/proxy", "app_proxy_unbind", "app", audit.RiskHigh},
		// Cert hub.
		{http.MethodPost, "/api/v1/certs/issue", "cert_issue", "cert", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/manual/:id/confirm", "cert_manual_dns_confirm", "cert", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/certs/manual/:id", "cert_manual_dns_cancel", "cert", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/import", "cert_import", "cert", audit.RiskHigh},
		{http.MethodPost, "/api/v1/certs/:id/renew", "cert_renew", "cert", audit.RiskHigh},
		{http.MethodDelete, "/api/v1/certs/:id", "cert_delete", "cert", audit.RiskHigh},
		// File ops.
		{http.MethodPost, "/api/v1/hosts/:id/files/mkdir", "file_mkdir", "host", ""},
		{http.MethodDelete, "/api/v1/hosts/:id/files", "file_remove", "host", audit.RiskHigh},
		{http.MethodPost, "/api/v1/hosts/:id/scans", "scan_trigger", "host", ""},
		// Backups.
		{http.MethodPost, "/api/v1/backups/jobs/:id/archives/:archiveID/restore", "backup_archive_restore", "backup", audit.RiskHigh},
		{http.MethodPost, "/api/v1/backups/s3-targets/:id/test", "backup_s3_test", "backup", audit.RiskLow},
		// Git provider.
		{http.MethodPost, "/api/v1/git/github/token", "git_token_set", "system", audit.RiskHigh},
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
		{http.MethodPost, "/api/v1/apps/webhook/:token"},
	}
	for _, s := range silent {
		if _, ok := deriveMutationAction(s[0], s[1]); ok {
			t.Errorf("%s %s: should not be audited", s[0], s[1])
		}
	}
}
