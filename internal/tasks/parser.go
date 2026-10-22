package tasks

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// IssueRequest is a parsed GitHub issue URL.
type IssueRequest struct {
	IssueURL    string
	Repo        string
	IssueNumber int
}

// PRRequest is a parsed GitHub PR URL.
type PRRequest struct {
	PRURL    string
	Repo     string
	PRNumber int
}

var githubIssuePattern = regexp.MustCompile(`\b(?:https?://(?:www\.)?)?github\.com/([^/\s]+)/([^/\s]+)/issues/([0-9]+)\b`)

var githubPRPattern = regexp.MustCompile(`\b(?:https?://(?:www\.)?)?github\.com/([^/\s]+)/([^/\s]+)/pull/([0-9]+)\b`)

// FindIssueRequest extracts a GitHub issue URL from text.
func FindIssueRequest(text string) (IssueRequest, error) {
	m := githubIssuePattern.FindStringSubmatch(text)
	if m == nil {
		return IssueRequest{}, errors.New("no GitHub issue URL found")
	}
	repo := m[1] + "/" + m[2]
	num, err := strconv.Atoi(m[3])
	if err != nil {
		return IssueRequest{}, fmt.Errorf("invalid issue number: %q", m[3])
	}
	return IssueRequest{
		IssueURL:    "https://github.com/" + repo + "/issues/" + m[3],
		Repo:        repo,
		IssueNumber: num,
	}, nil
}

// FindPRRequest extracts a GitHub PR URL from text.
func FindPRRequest(text string) (PRRequest, error) {
	m := githubPRPattern.FindStringSubmatch(text)
	if m == nil {
		return PRRequest{}, errors.New("no GitHub PR URL found")
	}
	repo := m[1] + "/" + m[2]
	num, err := strconv.Atoi(m[3])
	if err != nil {
		return PRRequest{}, fmt.Errorf("invalid PR number: %q", m[3])
	}
	return PRRequest{
		PRURL:    "https://github.com/" + repo + "/pull/" + m[3],
		Repo:     repo,
		PRNumber: num,
	}, nil
}
