package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	ID          int64  `json:"id"`
	User        string `json:"user"`
	State       string `json:"state"`
	Body        string `json:"body"`
	SubmittedAt string `json:"submitted_at"`
}

// ResolvePRReviews returns the submitted reviews of a PR, oldest first.
func ResolvePRReviews(ctx context.Context, ghCLI, repo string, num int) []PRReview {
	reviews, _ := FetchPRReviews(ctx, ghCLI, repo, num)
	return reviews
}

// FetchPRReviews decodes every paginated array and never hides a later-page error.
func FetchPRReviews(ctx context.Context, ghCLI, repo string, num int) ([]PRReview, error) {
	if ghCLI == "" {
		return nil, fmt.Errorf("GitHub CLI is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghCLI, "api", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, num), "--paginate",
		"--jq", `[.[] | select(.state != "PENDING") | {id: .id, user: .user.login, state: .state, body: .body, submitted_at: .submitted_at}]`)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 || bytes.TrimSpace(out)[0] != '[' {
		return nil, fmt.Errorf("invalid PR reviews payload")
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	reviews := []PRReview{}
	for {
		var page []PRReview
		if err := dec.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, fmt.Errorf("invalid PR reviews page")
		}
		for _, r := range page {
			if r.State != "PENDING" {
				reviews = append(reviews, r)
			}
		}
	}
	return reviews, nil
}

func IsGitDir(path string) bool {
	info, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil && info.IsDir()
}

// InlineComment is one inline review comment to post on a PR at a specific
// path:line of the diff.
type InlineComment struct {
	Path string
	Line int
	Body string
}

// PostInlineComment posts a single inline review comment on the PR head at
// path:line via `gh api`. The line must be part of the PR diff (added line or
// within a hunk's context); otherwise GitHub rejects it and the error is
// returned so callers can skip that finding without failing the review. On
// success it returns the GitHub comment id, which uniquely identifies the
// comment (used later to resolve the thread when the finding is addressed).
func PostInlineComment(ctx context.Context, ghCLI, repo string, num int, headCommit string, c InlineComment) (string, error) {
	if ghCLI == "" || c.Path == "" || c.Line <= 0 {
		return "", errors.New("cannot post inline comment: gh cli, path and line are required")
	}
	payload := map[string]any{
		"body": c.Body,
		"path": c.Path,
		"line": c.Line,
		"side": "RIGHT",
	}
	if headCommit != "" {
		payload["commit_id"] = headCommit
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, ghCLI, "api",
		fmt.Sprintf("repos/%s/pulls/%d/comments", repo, num),
		"--method", "POST", "--input", "-",
	)
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("post inline comment %s:%d: %w: %s", c.Path, c.Line, err, strings.TrimSpace(string(out)))
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || resp.ID == 0 {
		return "", fmt.Errorf("post inline comment %s:%d: no id in gh response: %s", c.Path, c.Line, strings.TrimSpace(string(out)))
	}
	return strconv.FormatInt(resp.ID, 10), nil
}

// ResolveInlineComment resolves the GitHub review thread for a previously
// posted inline comment. There is no REST endpoint for thread resolution, so
// this uses the GraphQL API: it locates the thread whose comments include the
// given comment databaseId and runs resolveReviewThread. Already-resolved or
// missing threads are treated as no-ops.
func ResolveInlineComment(ctx context.Context, ghCLI, repo string, num int, commentID string) error {
	if ghCLI == "" || commentID == "" {
		return errors.New("cannot resolve inline comment: gh cli and comment id are required")
	}
	owner, name := splitRepo(repo)

	findQuery := `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          comments(first: 100) { nodes { databaseId } }
        }
      }
    }
  }
}`
	cmd := exec.CommandContext(ctx, ghCLI, "api", "graphql",
		"-f", "query="+findQuery,
		"-F", "owner="+owner,
		"-F", "name="+name,
		"-F", "number="+strconv.Itoa(num),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("find review thread for comment %s: %w: %s", commentID, err, strings.TrimSpace(string(out)))
	}
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return fmt.Errorf("parse review thread lookup: %w", err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("find review thread: %s", resp.Errors[0].Message)
	}
	var threadID string
	for _, t := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if t.IsResolved {
			continue
		}
		for _, c := range t.Comments.Nodes {
			if strconv.FormatInt(c.DatabaseID, 10) == commentID {
				threadID = t.ID
				break
			}
		}
		if threadID != "" {
			break
		}
	}
	if threadID == "" {
		// Thread not found or already resolved; nothing to do.
		return nil
	}

	resolveQuery := `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`
	cmd = exec.CommandContext(ctx, ghCLI, "api", "graphql",
		"-f", "query="+resolveQuery,
		"-F", "threadId="+threadID,
	)
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("resolve review thread for comment %s: %w: %s", commentID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// splitRepo splits an "owner/name" repo string into its two parts.
func splitRepo(repo string) (string, string) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return repo, ""
	}
	return parts[0], parts[1]
}

// PostPRReview submits a formal PR review via `gh api`. event is one of
// APPROVE / REQUEST_CHANGES / COMMENT; body is the general review comment.
func PostPRReview(ctx context.Context, ghCLI, repo string, num int, event, body string) error {
	if ghCLI == "" {
		return errors.New("cannot post review: gh cli required")
	}
	payload := map[string]any{
		"event": event,
		"body":  body,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, ghCLI, "api",
		fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, num),
		"--method", "POST", "--input", "-",
	)
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("post PR review %s: %w: %s", event, err, strings.TrimSpace(string(out)))
	}
	return nil
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
