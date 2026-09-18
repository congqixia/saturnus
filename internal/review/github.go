package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
