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
	"time"
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
	info, _ := FetchIssueInfo(ctx, ghCLI, repo, num)
	return info
}

// FetchIssueInfo is the strict refresh lookup. Creation may use ResolveIssueInfo.
func FetchIssueInfo(ctx context.Context, ghCLI, repo string, num int) (IssueInfo, error) {
	var info IssueInfo
	out, err := ghOutput(ctx, ghCLI, "issue", "view", strconv.Itoa(num), "--repo", repo, "--json", "number,title,state,url")
	if err != nil {
		return info, err
	}
	if err = json.Unmarshal(out, &info); err != nil {
		return info, err
	}
	info.State = strings.ToUpper(info.State)
	if info.State != "OPEN" && info.State != "CLOSED" {
		return IssueInfo{}, fmt.Errorf("invalid issue state %q", info.State)
	}
	return info, nil
}

// ghOutput bounds each subprocess within the caller's remaining budget.
func ghOutput(ctx context.Context, ghCLI string, args ...string) ([]byte, error) {
	if ghCLI == "" {
		return nil, fmt.Errorf("GitHub CLI is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghCLI, args...)
	// Bound the wait for inherited pipes if a child outlives the GitHub process.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("GitHub lookup: %w", err)
	}
	return out, nil
}

// PRInfo is the subset of `gh pr view` output the module needs.
type PRInfo struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	URL        string `json:"url"`
	BaseBranch string `json:"baseRefName"`
	HeadCommit string `json:"headRefOid"`
}

// ResolvePRInfo resolves a pull request via `gh pr view`, returning zero
// values when gh is unavailable or the lookup fails.
func ResolvePRInfo(ctx context.Context, ghCLI, repo string, num int) PRInfo {
	info, _ := FetchPRInfo(ctx, ghCLI, repo, num)
	return info
}

func FetchPRInfo(ctx context.Context, ghCLI, repo string, num int) (PRInfo, error) {
	var info PRInfo
	out, err := ghOutput(ctx, ghCLI, "pr", "view", strconv.Itoa(num), "--repo", repo, "--json", "number,title,state,url,baseRefName,headRefOid")
	if err != nil {
		return info, err
	}
	if err = json.Unmarshal(out, &info); err != nil {
		return info, err
	}
	info.State = strings.ToUpper(info.State)
	if info.State != "OPEN" && info.State != "CLOSED" && info.State != "MERGED" {
		return PRInfo{}, fmt.Errorf("invalid PR state %q", info.State)
	}
	return info, nil
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
	prs, _ := FetchLinkedPRs(ctx, ghCLI, repo, issueNum)
	return prs
}

func FetchLinkedPRs(ctx context.Context, ghCLI, repo string, issueNum int) ([]LinkedPR, error) {
	out, err := ghOutput(ctx, ghCLI, "api", fmt.Sprintf("repos/%s/issues/%d/timeline?per_page=100", repo, issueNum), "--paginate")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 || bytes.TrimSpace(out)[0] != '[' {
		return nil, fmt.Errorf("invalid timeline payload")
	}
	seen := map[int]bool{}
	var prs []LinkedPR
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var events []timelineEvent
		if err := dec.Decode(&events); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if events == nil {
			return nil, fmt.Errorf("invalid timeline page")
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
	return prs, nil
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
