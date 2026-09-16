package review

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type PRRequest struct {
	PRURL    string
	Repo     string
	PRNumber int
}

var githubPRPattern = regexp.MustCompile(`\b(?:https?://(?:www\.)?)?github\.com/([^/\s]+)/([^/\s]+)/pull/([0-9]+)\b`)

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

func IsReviewListCommand(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "/sat reviews")
}
