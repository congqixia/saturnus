package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type PRInfo struct {
	Title      string `json:"title"`
	BaseBranch string `json:"baseRefName"`
	HeadCommit string `json:"headRefOid"`
	// State is the GitHub PR state: "OPEN", "CLOSED" or "MERGED".
	State string `json:"state"`
}

func ResolvePRInfo(ctx context.Context, ghCLI, repo string, num int) PRInfo {
	if ghCLI == "" {
		return PRInfo{}
	}
	cmd := exec.CommandContext(ctx, ghCLI, "pr", "view", strconv.Itoa(num), "--repo", repo, "--json", "title,baseRefName,headRefOid,state")
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

// PRReview is one submitted GitHub review (APPROVED / CHANGES_REQUESTED /
// COMMENTED). Pending reviews are filtered out.
type PRReview struct {
	User        string `json:"user"`
	State       string `json:"state"`
	Body        string `json:"body"`
	SubmittedAt string `json:"submitted_at"`
}

// ResolvePRReviews returns the submitted reviews of a PR, oldest first.
func ResolvePRReviews(ctx context.Context, ghCLI, repo string, num int) []PRReview {
	if ghCLI == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, ghCLI, "api",
		fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, num),
		"--jq", `[.[] | select(.state != "PENDING") | {user: .user.login, state: .state, body: .body, submitted_at: .submitted_at}]`,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var reviews []PRReview
	if err := json.Unmarshal(out, &reviews); err != nil {
		return nil
	}
	return reviews
}

func IsGitDir(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}

// OpenPR is one open pull request as reported by `gh pr list`. The BaseBranch
// is the PR's merge target, which distinguishes where the change lands; the
// HeadBranch is the source branch carrying the change.
type OpenPR struct {
	// Repo is the repository the PR belongs to (owner/name). It is injected by
	// ListOpenPRs because gh pr list is scoped to one repo per invocation.
	Repo       string `json:"-"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	HeadBranch string `json:"headRefName"`
	BaseBranch string `json:"baseRefName"`
	URL        string `json:"url"`
	IsDraft    bool   `json:"isDraft"`
	UpdatedAt  string `json:"updatedAt"`
}

// ListOpenPRs returns the open pull requests authored by the current gh user
// across the given repos (the review whitelist). One `gh pr list` call is made
// per repo; a repo whose lookup fails is reported in errs instead of aborting
// the whole sweep, so one broken repo does not hide the others.
func ListOpenPRs(ctx context.Context, ghCLI string, repos []string) ([]OpenPR, map[string]error) {
	errs := map[string]error{}
	if ghCLI == "" {
		return nil, errs
	}
	var out []OpenPR
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		cmd := exec.CommandContext(ctx, ghCLI, "pr", "list",
			"--repo", repo,
			"--author", "@me",
			"--state", "open",
			"--json", "number,title,headRefName,baseRefName,isDraft,url,updatedAt",
		)
		raw, err := cmd.Output()
		if err != nil {
			errs[repo] = err
			continue
		}
		var prs []OpenPR
		if err := json.Unmarshal(raw, &prs); err != nil {
			errs[repo] = err
			continue
		}
		for i := range prs {
			prs[i].Repo = repo
		}
		out = append(out, prs...)
	}
	return out, errs
}
