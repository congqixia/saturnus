// Package ghinfo inspects GitHub CLI and effective authentication metadata.
// Tokens themselves are never requested, retained or reported.
package ghinfo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const Host = "github.com"

// Info is safe to include in registrations and heartbeats. Missing expiration
// means unknown, not non-expiring. Refreshable credentials have two lifetimes.
type Info struct {
	Version               string     `json:"version"`
	Host                  string     `json:"host"`
	CheckedAt             time.Time  `json:"checked_at"`
	AuthStatus            string     `json:"auth_status"`
	ExpiryStatus          string     `json:"expiry_status"`
	ExpirySource          string     `json:"expiry_source,omitempty"`
	TokenExpiresAt        *time.Time `json:"token_expires_at,omitempty"`
	Refreshable           bool       `json:"refreshable,omitempty"`
	RefreshTokenExpiresAt *time.Time `json:"refresh_token_expires_at,omitempty"`
}

// Probe requires gh --version to work. Auth failures are reported as metadata,
// allowing registration even when expiration cannot be discovered.
func Probe(ctx context.Context, cli string) (*Info, error) {
	out, err := output(ctx, cli, "--version")
	if err != nil {
		return nil, fmt.Errorf("check GitHub CLI version: %w", err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "gh" || fields[1] != "version" {
		return nil, errors.New("check GitHub CLI version: unrecognized gh --version output")
	}
	info := &Info{Version: fields[2], Host: Host, CheckedAt: time.Now().UTC(), AuthStatus: "unknown", ExpiryStatus: "unknown"}
	// New gh versions expose expiry for automatically refreshed OAuth tokens.
	// Never use --show-token or read credential storage.
	out, err = output(ctx, cli, "auth", "status", "--active", "--hostname", Host, "--json", "hosts")
	if err == nil {
		var status struct {
			Hosts map[string][]struct {
				Active                bool       `json:"active"`
				State                 string     `json:"state"`
				Refreshable           bool       `json:"refreshable"`
				TokenExpiresAt        *time.Time `json:"tokenExpiresAt"`
				RefreshTokenExpiresAt *time.Time `json:"refreshTokenExpiresAt"`
			} `json:"hosts"`
		}
		if json.Unmarshal(out, &status) == nil {
			for _, account := range status.Hosts[Host] {
				if !account.Active {
					continue
				}
				info.Refreshable = account.Refreshable
				info.TokenExpiresAt = utcTime(account.TokenExpiresAt)
				info.RefreshTokenExpiresAt = utcTime(account.RefreshTokenExpiresAt)
				if account.State == "success" {
					info.AuthStatus = "valid"
				} else {
					info.AuthStatus = "invalid"
				}
				if info.TokenExpiresAt != nil || info.RefreshTokenExpiresAt != nil {
					info.ExpiryStatus, info.ExpirySource = "known", "gh_auth_status"
				}
				if info.Refreshable || info.ExpiryStatus == "known" {
					return info, nil
				}
			}
		}
	}
	// PAT expiry is supplied in an authenticated GitHub response header.
	// --silent suppresses the body, while --include retains response headers.
	out, err = output(ctx, cli, "api", "--hostname", Host, "--include", "--silent", "user")
	if err != nil && len(out) == 0 {
		info.ExpiryStatus = "check_failed"
		return info, nil
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), nil)
	if err != nil {
		info.ExpiryStatus = "check_failed"
		return info, nil
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		info.ExpiryStatus = "check_failed"
		if resp.StatusCode == http.StatusUnauthorized {
			info.AuthStatus = "invalid"
		}
		return info, nil
	}
	info.AuthStatus = "valid"
	expires := strings.TrimSpace(resp.Header.Get("GitHub-Authentication-Token-Expiration"))
	if expires == "" {
		return info, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05 MST", http.TimeFormat} {
		if expiry, err := time.Parse(layout, expires); err == nil {
			info.TokenExpiresAt = utcTime(&expiry)
			info.ExpiryStatus, info.ExpirySource = "known", "github_response_header"
			return info, nil
		}
	}
	info.ExpiryStatus = "check_failed"
	return info, nil
}

func utcTime(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func output(ctx context.Context, cli string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_PAGER=cat", "NO_COLOR=1")
	cmd.WaitDelay = time.Second
	// Keep stderr private: auth/debug errors may contain sensitive data.
	return cmd.Output()
}
