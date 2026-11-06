package repopolicy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGuardCollisionPolicies(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                   string
		policy                 Policy
		repo, checkout, review string
		blocked                bool
	}{
		{"default same repo", "", "a/b", t.TempDir(), "two", true},
		{"mutex same repo", Mutex, "a/b", dir, "two", true},
		{"mutex symlink alias", Mutex, "other/repo", alias, "two", true},
		{"mutex different repo", Mutex, "other/repo", t.TempDir(), "two", false},
		{"worktree same repo", Worktree, "a/b", dir, "two", false},
		{"worktree same review", Worktree, "a/b", dir, "one", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var g Guard
			unlock, err := g.Lock(context.Background(), tc.policy, "a/b", dir, "one")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			release, err := g.Lock(ctx, tc.policy, tc.repo, tc.checkout, tc.review)
			if tc.blocked {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expected blocked acquisition, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				release()
			}
			unlock()
			// Cancellation must release any earlier keys and allow a later job.
			ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
			defer cancel2()
			release, err = g.Lock(ctx2, tc.policy, tc.repo, tc.checkout, tc.review)
			if err != nil {
				t.Fatalf("lock leaked: %v", err)
			}
			release()
		})
	}
}

func TestParse(t *testing.T) {
	for raw, want := range map[string]Policy{"": Mutex, "mutex": Mutex, "worktree": Worktree, " worktree ": Worktree} {
		if got, err := Parse(raw); err != nil || got != want {
			t.Fatalf("Parse(%q) = %q, %v", raw, got, err)
		}
	}
	if _, err := Parse("unsafe"); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
