package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"saturnus/internal/lark"
	"saturnus/internal/repoconfig"
	"saturnus/internal/review"
)

func TestDisableLocalReviewFallbackKeepsRepoMappings(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	repos := map[string]string{"owner/repo": root}
	for _, value := range []string{"", "0", "1"} {
		t.Setenv("SATURNUS_REVIEW_LOCAL_FALLBACK", value)
		cfg := reviewConfig(repos)
		svc := review.New(nil, lark.New(lark.Config{}), cfg)
		if svc.LocalAvailable("owner/repo") != (value != "0") || cfg.Repos["owner/repo"] != root {
			t.Fatalf("local fallback setting %q lost its mapping or was ignored", value)
		}
	}
}

func TestSharedRepoFileConfig(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "repos.json")
	if err := os.WriteFile(path, []byte(`{"owner/repo":"checkout,with=punctuation"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	repos, err := repoconfig.Load("", path)
	if err != nil {
		t.Fatal(err)
	}
	// Module config builders must use the startup snapshot rather than parse
	// the environment again, so Tasks and review cannot see different mappings.
	t.Setenv("SATURNUS_REVIEW_REPOS", "other/repo=/other")
	t.Setenv("SATURNUS_TASKS_REFRESH_INTERVAL", "")
	t.Setenv("SATURNUS_TASKS_REFRESH_TIMEOUT", "")
	reviewCfg := reviewConfig(repos)
	taskCfg, err := tasksConfig(repos)
	want := map[string]string{"owner/repo": filepath.Join(base, "checkout,with=punctuation")}
	if err != nil || !reflect.DeepEqual(reviewCfg.Repos, want) || !reflect.DeepEqual(taskCfg.Repos, want) {
		t.Fatalf("review=%v tasks=%v err=%v, want %v", reviewCfg.Repos, taskCfg.Repos, err, want)
	}
}

func TestRunRejectsRepoConfigBeforeOpeningStore(t *testing.T) {
	for _, tc := range []struct {
		name, inline, content, message string
	}{
		{"invalid file", "", `{"owner/repo":null}`, "non-empty string"},
		{"conflicting sources", "owner/repo=/checkout", `{}`, "sources conflict"},
		{"missing file", "", "", "read repo config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			path := filepath.Join(base, "repos.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("SATURNUS_REVIEW_ENABLED", "0")
			t.Setenv("SATURNUS_TASKS_ENABLED", "0")
			t.Setenv("SATURNUS_REVIEW_REPOS", tc.inline)
			t.Setenv("SATURNUS_REVIEW_REPOS_FILE", path)
			dbPath := filepath.Join(base, "saturnus.db")
			err := run(context.Background(), "127.0.0.1:0", dbPath, "")
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("run err=%v, want %q", err, tc.message)
			}
			if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
				t.Fatalf("store opened before config validation: %v", err)
			}
		})
	}
}
