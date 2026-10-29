package prs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"saturnus/internal/store"
)

// Policy matches exact, case-sensitive check names. Repo rules add to globals.
type Policy struct {
	Global []string            `json:"global"`
	Repos  map[string][]string `json:"repos"`
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ValidRepo(repo string) bool {
	if !repoPattern.MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func LoadPolicy(file string) (Policy, error) {
	p := Policy{Repos: map[string][]string{}}
	if file == "" {
		return p, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return p, fmt.Errorf("read PR bypass config: %w", err)
	}
	if data := bytes.TrimSpace(raw); len(data) == 0 || data[0] != '{' {
		return p, fmt.Errorf("PR bypass config must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&p); err != nil {
		return p, fmt.Errorf("decode PR bypass config: %w", err)
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return p, fmt.Errorf("PR bypass config must contain one JSON object")
	}
	if p.Repos == nil {
		p.Repos = map[string][]string{}
	}
	validate := func(names []string) error {
		seen := map[string]bool{}
		for _, name := range names {
			if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
				return fmt.Errorf("check names must be non-empty without surrounding whitespace")
			}
			if seen[name] {
				return fmt.Errorf("duplicate check name %q", name)
			}
			seen[name] = true
		}
		return nil
	}
	if err := validate(p.Global); err != nil {
		return p, err
	}
	normalized := map[string][]string{}
	for repo, names := range p.Repos {
		if !ValidRepo(repo) {
			return p, fmt.Errorf("invalid bypass repo %q", repo)
		}
		if err := validate(names); err != nil {
			return p, fmt.Errorf("repo %s: %w", repo, err)
		}
		key := strings.ToLower(repo)
		if _, ok := normalized[key]; ok {
			return p, fmt.Errorf("duplicate bypass repo %q", repo)
		}
		normalized[key] = names
	}
	p.Repos = normalized
	return p, nil
}

func (p Policy) Rule(repo, name string) string {
	for _, n := range p.Repos[strings.ToLower(repo)] {
		if n == name {
			return "repo:" + strings.ToLower(repo)
		}
	}
	for _, n := range p.Global {
		if n == name {
			return "global"
		}
	}
	return ""
}

// Evaluate changes only local policy fields. Upstream status/conclusion/bucket
// stay visible; bypassed items never contribute to the effective result.
func (p Policy) Evaluate(repo string, checks []store.PRCheck) (string, store.PRCheckCounts) {
	counts := store.PRCheckCounts{}
	for i := range checks {
		c := &checks[i]
		c.BypassRule = p.Rule(repo, c.Name)
		c.Bypassed = c.BypassRule != ""
		if c.Bypassed {
			counts.Bypass++
			continue
		}
		switch c.Bucket {
		case "pass":
			counts.Pass++
		case "fail":
			counts.Fail++
		case "pending":
			counts.Pending++
		case "cancel":
			counts.Cancel++
		case "skip":
			counts.Skip++
		default:
			counts.Unknown++
		}
	}
	switch {
	case len(checks) == 0:
		return "NO_CHECKS", counts
	case counts.Fail > 0:
		return "FAIL", counts
	case counts.Unknown > 0:
		return "UNKNOWN", counts
	case counts.Pending > 0:
		return "PENDING", counts
	case counts.Cancel > 0:
		return "CANCEL", counts
	case counts.Pass > 0:
		return "PASS", counts
	case counts.Skip > 0:
		return "SKIP", counts
	default:
		return "BYPASS", counts
	}
}
