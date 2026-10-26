package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// LinkedPR is a pull request discovered from an issue's timeline.
type LinkedPR struct {
	Number int
	URL    string
}

// timelineEvent is one item of the GitHub issue timeline REST response. Only
// the fields needed to identify a cross-referenced PR are kept.
type timelineEvent struct {
	Event  string `json:"event"`
	Source *struct {
		Issue *struct {
			Number        int                        `json:"number"`
			RepositoryURL string                     `json:"repository_url"`
			PullRequest   map[string]json.RawMessage `json:"pull_request"`
		} `json:"issue"`
	} `json:"source"`
}

// ResolveLinkedPRs discovers PRs linked to a GitHub issue by scanning the
// issue timeline for cross-referenced events. In the timeline payload a PR is
// a PR-as-issue, so an event whose source issue carries a pull_request field is
// a PR; plain issue mentions and references from forks or other repositories
// (bots, dependency repos) are filtered out. The result is deduped by PR
// number and returned in timeline order. It returns nil when gh is unavailable
// or the lookup fails.
func ResolveLinkedPRs(ctx context.Context, ghCLI, repo string, issueNum int) []LinkedPR {
	if ghCLI == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, ghCLI, "api",
		fmt.Sprintf("repos/%s/issues/%d/timeline?per_page=100", repo, issueNum), "--paginate")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var prs []LinkedPR
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var events []timelineEvent
		if err := dec.Decode(&events); err == io.EOF {
			break
		} else if err != nil {
			return nil
		}
		for _, ev := range events {
			if ev.Event != "cross-referenced" || ev.Source == nil || ev.Source.Issue == nil {
				continue
			}
			iss := ev.Source.Issue
			if iss.PullRequest == nil || !sameRepo(iss.RepositoryURL, repo) {
				continue
			}
			if seen[iss.Number] {
				continue
			}
			seen[iss.Number] = true
			prs = append(prs, LinkedPR{Number: iss.Number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, iss.Number)})
		}
	}
	return prs
}

// sameRepo reports whether an API repository URL points at the given
// owner/repo, ignoring case.
func sameRepo(apiURL, repo string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimRight(apiURL, "/")), strings.ToLower("/repos/"+repo))
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
