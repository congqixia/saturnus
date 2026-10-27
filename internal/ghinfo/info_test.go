package ghinfo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeGH(t *testing.T, version, status, headers string, failAPI bool) string {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{"version": version, "status": status, "headers": headers} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
dir=$(dirname "$0")
printf '%s\n' "$*" >> "$dir/calls"
case "$*" in
--version) cat "$dir/version" ;;
"auth status --active --hostname github.com --json hosts") cat "$dir/status" ;;
"api --hostname github.com --include --silent user") cat "$dir/headers"
`
	if failAPI {
		script += "echo 'secret token should never be reported' >&2\nexit 1\n"
	}
	script += ";;\n*) exit 1 ;;\nesac\n"
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbePATExpirationHeaders(t *testing.T) {
	for _, value := range []string{"2026-11-01 10:00:00 +0800", "2026-11-01T02:00:00Z", "2026-11-01 02:00:00 UTC"} {
		t.Run(value, func(t *testing.T) {
			gh := fakeGH(t, "gh version 2.86.0 (2026-01-01)\nhttps://github.com/cli/cli\n", `{"hosts":{"github.com":[{"active":true,"state":"success","token":"masked-secret"}]}}`, "HTTP/2.0 200 OK\r\nGitHub-Authentication-Token-Expiration: "+value+"\r\n\r\n", false)
			info, err := Probe(context.Background(), gh)
			want := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
			if err != nil || info.Version != "2.86.0" || info.Host != Host || info.TokenExpiresAt == nil || !info.TokenExpiresAt.Equal(want) || info.ExpirySource != "github_response_header" || info.AuthStatus != "valid" || info.ExpiryStatus != "known" || info.CheckedAt.IsZero() {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			raw, _ := json.Marshal(info)
			if strings.Contains(string(raw), "secret") {
				t.Fatalf("credential value leaked into metadata: %s", raw)
			}
		})
	}
}

func TestProbeRefreshableCredentialSelectsActiveAccount(t *testing.T) {
	status := `{"hosts":{"github.com":[{"active":false,"state":"success","tokenExpiresAt":"2026-10-28T00:00:00Z"},{"active":true,"state":"success","refreshable":true,"tokenExpiresAt":"2026-10-27T16:00:00Z","refreshTokenExpiresAt":"2027-01-01T00:00:00Z","token":"masked-secret"}]}}`
	gh := fakeGH(t, "gh version 2.103.0\n", status, "", false)
	info, err := Probe(context.Background(), gh)
	if err != nil || !info.Refreshable || info.TokenExpiresAt == nil || info.RefreshTokenExpiresAt == nil || info.ExpirySource != "gh_auth_status" {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	calls, err := os.ReadFile(filepath.Join(filepath.Dir(gh), "calls"))
	if err != nil || strings.Contains(string(calls), "api ") || strings.Contains(string(calls), "--show-token") {
		t.Fatalf("unexpected credential commands: %s err=%v", calls, err)
	}
}

func TestProbeUnknownAndFailedExpiry(t *testing.T) {
	for _, tc := range []struct {
		name, headers, want, auth string
		fail                      bool
	}{
		{"no header", "HTTP/2.0 200 OK\r\n\r\n", "unknown", "valid", false},
		{"malformed date", "HTTP/2.0 200 OK\r\nGitHub-Authentication-Token-Expiration: invalid\r\n\r\n", "check_failed", "valid", false},
		{"expired auth", "HTTP/2.0 401 Unauthorized\r\n\r\n", "check_failed", "invalid", true},
		{"network failure", "", "check_failed", "unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Old gh auth JSON is unsupported; PAT headers remain usable.
			gh := fakeGH(t, "gh version 2.80.0\n", "unsupported JSON", tc.headers, tc.fail)
			info, err := Probe(context.Background(), gh)
			if err != nil || info.ExpiryStatus != tc.want || info.AuthStatus != tc.auth || info.TokenExpiresAt != nil {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			raw, _ := json.Marshal(info)
			if strings.Contains(string(raw), "secret") {
				t.Fatal("stderr leaked into registration metadata")
			}
		})
	}
}

func TestProbeRequiresWorkingVersion(t *testing.T) {
	if _, err := Probe(context.Background(), filepath.Join(t.TempDir(), "missing-gh")); err == nil {
		t.Fatal("accepted missing gh")
	}
	if _, err := Probe(context.Background(), fakeGH(t, "unknown", "", "", false)); err == nil {
		t.Fatal("accepted unrecognized version")
	}
}
