// Package repopolicy defines checkout collision policies for review executors.
package repopolicy

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"saturnus/internal/keylock"
)

type Policy string

const (
	Mutex    Policy = "mutex"
	Worktree Policy = "worktree"
)

// Parse defaults to mutual exclusion. Invalid configuration must never silently
// turn off checkout protection.
func Parse(raw string) (Policy, error) {
	switch p := Policy(strings.TrimSpace(raw)); p {
	case "", Mutex:
		return Mutex, nil
	case Worktree:
		return Worktree, nil
	default:
		return "", fmt.Errorf("invalid repo collision policy %q: expected mutex or worktree", raw)
	}
}

// CheckoutKey recognizes relative paths and symlink aliases on the executor.
// The scheduler uses Clean on worker paths instead: remote paths are not local.
func CheckoutKey(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Clean(dir)
}

// Guard serializes jobs within an executor. All modes protect a review's own
// session/worktree; mutex additionally protects the repo and checkout (including
// different repo mappings that point at the same directory). Waiting is
// cancellable and callers hold the release function until execution has ended.
type Guard struct{ locks keylock.Map }

func (g *Guard) Lock(ctx context.Context, policy Policy, repo, dir, reviewID string) (func(), error) {
	p, err := Parse(string(policy))
	if err != nil {
		return nil, err
	}
	keys := []string{"review:" + reviewID}
	if p == Mutex {
		keys = append(keys, "repo:"+strings.ToLower(repo), "checkout:"+CheckoutKey(dir))
	}
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, key := range keys {
		unlock, err := g.locks.Lock(ctx, key)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, unlock)
	}
	return release, nil
}
