// Package repoconfig loads host-local repository checkout mappings shared by
// the review executor, remote workers and Tasks module.
package repoconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Load accepts either the legacy comma-separated mapping or a JSON file, but
// never both. File paths are resolved relative to the config file's directory;
// inline paths retain their legacy semantics. No checkout access is required.
func Load(inline, file string) (map[string]string, error) {
	if file == "" {
		return ParseInline(inline), nil
	}
	if strings.TrimSpace(inline) != "" {
		return nil, errors.New("repository mapping sources conflict: use either SATURNUS_REVIEW_REPOS (-repos) or SATURNUS_REVIEW_REPOS_FILE (-repos-file), not both")
	}
	path, err := filepath.Abs(file)
	if err != nil {
		return nil, fmt.Errorf("resolve repo config %q: %w", file, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read repo config %q: %w", path, err)
	}
	repos, err := parseFile(data, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("parse repo config %q: %w", path, err)
	}
	return repos, nil
}

func parseFile(data []byte, baseDir string) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, errors.New("expected a JSON object mapping owner/repo to checkout path")
	}
	repos := make(map[string]string)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		repo, ok := token.(string)
		if !ok || !repoName.MatchString(repo) {
			return nil, fmt.Errorf("invalid repo %q: expected owner/repo", token)
		}
		owner, name, _ := strings.Cut(repo, "/")
		if owner == "." || owner == ".." || name == "." || name == ".." {
			return nil, fmt.Errorf("invalid repo %q: expected owner/repo", repo)
		}
		if _, exists := repos[repo]; exists {
			return nil, fmt.Errorf("duplicate repo %q", repo)
		}
		var dir string
		if err := dec.Decode(&dir); err != nil {
			return nil, fmt.Errorf("repo %q: expected a checkout path string: %w", repo, err)
		}
		if strings.TrimSpace(dir) == "" {
			return nil, fmt.Errorf("repo %q: checkout path must be a non-empty string", repo)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(baseDir, dir)
		}
		repos[repo] = filepath.Clean(dir)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("unexpected content after repository mapping")
	}
	return repos, nil
}

// ParseInline preserves the legacy format: invalid entries are skipped and the
// last occurrence of a repo wins. Paths may contain equals signs, but not commas.
func ParseInline(value string) map[string]string {
	out := map[string]string{}
	for _, item := range strings.Split(value, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) != 2 {
			continue
		}
		repo := strings.TrimSpace(parts[0])
		dir := strings.TrimSpace(parts[1])
		if repo != "" && dir != "" {
			out[repo] = dir
		}
	}
	return out
}
