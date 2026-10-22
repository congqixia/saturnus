package tasks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// IssueInfo is the subset of `gh issue view` output the module needs.
type IssueInfo struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	URL    string `json:"url"`
}

// ResolveIssueInfo resolves a GitHub issue via `gh issue view`, returning zero
// values when gh is unavailable or the lookup fails (the caller keeps the URL
// as a fallback).
func ResolveIssueInfo(ctx context.Context, ghCLI, repo string, num int) IssueInfo {
	if ghCLI == "" {
		return IssueInfo{}
	}
	cmd := exec.CommandContext(ctx, ghCLI, "issue", "view", strconv.Itoa(num), "--repo", repo, "--json", "number,title,state,url")
	out, err := cmd.Output()
	if err != nil {
		return IssueInfo{}
	}
	var info IssueInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return IssueInfo{}
	}
	return info
}

// PRInfo is the subset of `gh pr view` output the module needs.
type PRInfo struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	URL        string `json:"url"`
	BaseBranch string `json:"baseRefName"`
}

// ResolvePRInfo resolves a pull request via `gh pr view`, returning zero
// values when gh is unavailable or the lookup fails.
func ResolvePRInfo(ctx context.Context, ghCLI, repo string, num int) PRInfo {
	if ghCLI == "" {
		return PRInfo{}
	}
	cmd := exec.CommandContext(ctx, ghCLI, "pr", "view", strconv.Itoa(num), "--repo", repo, "--json", "number,title,state,url,baseRefName")
	out, err := cmd.Output()
	if err != nil {
		return PRInfo{}
	}
	var info PRInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return PRInfo{}
	}
	return info
}

// IsGitDir reports whether path is a git checkout root.
func IsGitDir(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}

// repoPath parses a "owner/repo:path" doc reference into its repo and
// repo-relative path parts.
func repoPath(ref string) (repo, path string, ok bool) {
	ref = strings.TrimSpace(ref)
	colon := strings.LastIndex(ref, ":")
	if colon < 0 {
		return "", "", false
	}
	repo = strings.TrimSpace(ref[:colon])
	path = strings.TrimSpace(ref[colon+1:])
	if repo == "" || path == "" || !strings.Contains(repo, "/") {
		return "", "", false
	}
	return repo, path, true
}

// FormatRepoPath renders a repo-relative doc path with its repo prefix.
func FormatRepoPath(repo, path string) string {
	return repo + ":" + path
}
