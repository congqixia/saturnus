package review

import (
	"fmt"
	"path/filepath"

	"saturnus/internal/repopolicy"
)

// IsolationPrompt is repeated for initial reviews, continuations and actions:
// a resumed tool session may still remember instructions to use the checkout.
// Saturnus deliberately leaves git worktree operations to the review tool.
func IsolationPrompt(policy repopolicy.Policy, checkout, reviewID string) string {
	if policy != repopolicy.Worktree {
		return ""
	}
	checkout = repopolicy.CheckoutKey(checkout)
	worktree := filepath.Join(filepath.Dir(checkout), ".saturnus-worktrees", filepath.Base(checkout), reviewID)
	return fmt.Sprintf(`Workspace isolation rules (take precedence over checkout instructions elsewhere in this prompt or previous conversation):
The shared repository checkout is %q. Other review tasks may be running against this repository concurrently. Perform ALL review, build, test and edit operations in the exclusive git worktree at %q for review %s.
Before doing any work, create that worktree with git worktree add --detach if it does not exist, or verify it belongs to this repository before reusing it. Fetch the requested PR head and check it out detached inside this worktree; on a retry or continuation verify/update the PR head there again. Use private refs if fetching requires a local ref; never overwrite shared branches or refs used by another task.
Never switch branches, reset, clean, stash or edit files in the shared checkout. Never use, remove or prune another review's worktree. Keep this review's worktree for subsequent retries and actions. If isolation cannot be established, stop and report the failure instead of falling back to the shared checkout.

`, checkout, worktree, reviewID)
}
