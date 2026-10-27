package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"saturnus/internal/keylock"
	"saturnus/internal/repoconfig"
	"saturnus/internal/store"
)

// Config controls the Tasks module. The repo whitelist is shared with the
// review module: only repos mapped to a local checkout can host issues, docs
// and PRs.
type Config struct {
	// Context cancels manual and scheduled refreshes when the server stops.
	Context         context.Context
	Enabled         bool
	GHCli           string
	RefreshInterval time.Duration
	RefreshTimeout  time.Duration
	// SyncReview receives previously fetched metadata and never starts a review.
	SyncReview func(context.Context, string, int, PRInfo) error
	// Repos maps owner/repo -> local checkout path, loaded from the shared
	// SATURNUS_REVIEW_REPOS or SATURNUS_REVIEW_REPOS_FILE configuration.
	Repos map[string]string
}

// ErrArchived is returned when operating on an archived (closed) task.
var ErrArchived = errors.New("task is archived")

// ErrDisabled is returned when the Tasks module is not enabled.
var ErrDisabled = errors.New("tasks module is disabled")

type Service struct {
	store *store.Store
	cfg   Config
	locks keylock.Map
}

func New(st *store.Store, cfg Config) *Service {
	if cfg.GHCli == "" {
		cfg.GHCli = "gh"
	}
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	if cfg.RefreshTimeout <= 0 {
		cfg.RefreshTimeout = time.Minute
	}
	return &Service{store: st, cfg: cfg}
}

// ParseRepoMap parses a comma-separated "owner/repo=/local/path" whitelist into
// a map. Invalid entries are skipped.
func ParseRepoMap(value string) map[string]string {
	return repoconfig.ParseInline(value)
}

func (s *Service) Enabled() bool {
	return s.cfg.Enabled
}

// GHCli returns the configured GitHub CLI executable (default "gh").
func (s *Service) GHCli() string {
	return s.cfg.GHCli
}

// RepoList returns the whitelisted repo names (owner/name).
func (s *Service) RepoList() []string {
	repos := make([]string, 0, len(s.cfg.Repos))
	for repo := range s.cfg.Repos {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

// Create binds a new task to a GitHub issue URL. The issue metadata is resolved
// via `gh issue view` when available; otherwise the URL itself is kept.
func (s *Service) Create(issueURL string) (store.Task, error) {
	if !s.cfg.Enabled {
		return store.Task{}, ErrDisabled
	}
	req, err := FindIssueRequest(issueURL)
	if err != nil {
		return store.Task{}, err
	}
	if _, ok := s.cfg.Repos[req.Repo]; !ok {
		return store.Task{}, fmt.Errorf("repo is not in the task whitelist: %s", req.Repo)
	}
	title := fmt.Sprintf("%s#%d", req.Repo, req.IssueNumber)
	issueTitle, issueState := "", ""
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info := ResolveIssueInfo(ctx, s.cfg.GHCli, req.Repo, req.IssueNumber)
	if info.Title != "" {
		title = info.Title
		issueTitle = info.Title
		issueState = info.State
	}
	task := store.Task{
		Title:       title,
		Status:      store.TaskStatusOpen,
		IssueRepo:   req.Repo,
		IssueNumber: req.IssueNumber,
		IssueTitle:  issueTitle,
		IssueState:  issueState,
		IssueURL:    req.IssueURL,
	}
	return s.store.CreateTask(task)
}

func (s *Service) Get(id string) (store.Task, error) {
	return s.store.GetTask(id)
}

// List returns tasks, newest first. Archived tasks are excluded unless
// includeArchived is true (or status is "archived").
func (s *Service) List(status string, includeArchived bool) []store.Task {
	return s.store.ListTasks(status, includeArchived)
}

// Close archives a task, hiding it from default listings and rejecting further
// attachments.
func (s *Service) Close(id string) (store.Task, error) {
	if !s.cfg.Enabled {
		return store.Task{}, ErrDisabled
	}
	return s.store.CloseTask(id)
}

// Docs returns the design docs attached to a task.
func (s *Service) Docs(taskID string) []store.TaskDoc {
	return s.store.ListTaskDocs(taskID)
}

// PRs returns the linked PRs of a task.
func (s *Service) PRs(taskID string) []store.TaskPR {
	return s.store.ListTaskPRs(taskID)
}

// AddDoc attaches a local design doc. ref is "owner/repo:path" (repo-relative
// path inside the mapped checkout). The file must exist in the checkout.
func (s *Service) AddDoc(taskID, ref string) (store.TaskDoc, error) {
	if !s.cfg.Enabled {
		return store.TaskDoc{}, ErrDisabled
	}
	if _, err := s.store.GetTask(taskID); err != nil {
		return store.TaskDoc{}, err
	}
	repo, path, ok := repoPath(ref)
	if !ok {
		return store.TaskDoc{}, errors.New("doc reference must be owner/repo:path, e.g. acme/foo:docs/plan/bar.md")
	}
	dir, ok := s.cfg.Repos[repo]
	if !ok {
		return store.TaskDoc{}, fmt.Errorf("repo is not in the task whitelist: %s", repo)
	}
	if dir == "" || !IsGitDir(dir) {
		return store.TaskDoc{}, fmt.Errorf("no local checkout configured for repo %s", repo)
	}
	full := filepath.Join(dir, filepath.FromSlash(path))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return store.TaskDoc{}, fmt.Errorf("design doc not found in checkout: %s (%s)", path, repo)
	}
	doc := store.TaskDoc{
		TaskID: taskID,
		Repo:   repo,
		Path:   path,
		Title:  filepath.Base(path),
	}
	return s.store.AddTaskDoc(doc)
}

func (s *Service) RemoveDoc(id string) error {
	if !s.cfg.Enabled {
		return ErrDisabled
	}
	return s.store.RemoveTaskDoc(id)
}

// AddPR links a PR to a task and snapshots its progress. When a review already
// exists for the PR (via review_requests), its status and Feishu task are
// copied into the snapshot; otherwise review_status is "none". No review is
// started here.
func (s *Service) AddPR(taskID, prURL string) (store.TaskPR, error) {
	if !s.cfg.Enabled {
		return store.TaskPR{}, ErrDisabled
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RefreshTimeout)
	defer cancel()
	stop := context.AfterFunc(s.cfg.Context, cancel)
	defer stop()
	unlock, err := s.locks.Lock(ctx, "task:"+taskID)
	if err != nil {
		return store.TaskPR{}, err
	}
	defer unlock()

	task, err := s.store.GetTask(taskID)
	if err != nil {
		return store.TaskPR{}, err
	}
	if task.Status == store.TaskStatusArchived {
		return store.TaskPR{}, ErrArchived
	}
	req, err := FindPRRequest(prURL)
	if err != nil {
		return store.TaskPR{}, err
	}
	if _, ok := s.cfg.Repos[req.Repo]; !ok {
		return store.TaskPR{}, fmt.Errorf("repo is not in the task whitelist: %s", req.Repo)
	}
	info, lookupErr := FetchPRInfo(ctx, s.cfg.GHCli, req.Repo, req.PRNumber)
	pr := store.TaskPR{
		TaskID:   taskID,
		Repo:     req.Repo,
		PRNumber: req.PRNumber,
		PRURL:    req.PRURL,
		PRTitle:  info.Title,
		PRState:  info.State,
	}
	if info.BaseBranch != "" {
		pr.BaseBranch = info.BaseBranch
	}
	if lookupErr != nil {
		existing, err := s.store.TaskPRsForRefresh(ctx, taskID)
		if err != nil {
			return store.TaskPR{}, err
		}
		for _, old := range existing {
			if old.Repo == req.Repo && old.PRNumber == req.PRNumber {
				pr.PRTitle, pr.PRState, pr.BaseBranch, pr.SyncedAt = old.PRTitle, old.PRState, old.BaseBranch, old.SyncedAt
			}
		}
	}
	pr.SyncAttemptedAt = time.Now().UTC()
	if lookupErr == nil {
		pr.SyncedAt = pr.SyncAttemptedAt
	} else {
		pr.SyncError = lookupErr.Error()
	}
	if err := s.snapshotReview(&pr, req.Repo, req.PRNumber); err != nil {
		return store.TaskPR{}, err
	}
	return s.store.AddTaskPRContext(ctx, pr, false)
}

// snapshotReview copies the linked review's status and Feishu task references
// into a TaskPR snapshot.
func (s *Service) snapshotReview(pr *store.TaskPR, repo string, num int) error {
	review, err := s.store.GetReviewByPR(repo, num)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		pr.ReviewID = ""
		pr.ReviewStatus = "none"
		pr.FsTaskID = ""
		pr.FsTaskURL = ""
		return nil
	}
	pr.ReviewID = review.ID
	pr.ReviewStatus = review.Status
	pr.FsTaskID = review.TaskID
	pr.FsTaskURL = review.TaskURL
	return nil
}

func (s *Service) RemovePR(id string) error {
	if !s.cfg.Enabled {
		return ErrDisabled
	}
	return s.store.RemoveTaskPR(id)
}

// Refresh is the compatibility wrapper for bot callers.
func (s *Service) Refresh(id string) (store.Task, []store.TaskPR, string, error) {
	result, err := s.RefreshContext(context.Background(), id)
	return result.Task, result.PRs, result.Suggestion, err
}

// RefreshAll preserves the manual non-archived scope, including done tasks.
func (s *Service) RefreshAll() ([]store.Task, map[string]string, error) {
	results, err := s.Sweep(context.Background(), false)
	if err != nil {
		return nil, nil, err
	}
	out := []store.Task{}
	notes := map[string]string{}
	for _, result := range results {
		if result.Task.ID != "" {
			out = append(out, result.Task)
		}
		note := strings.Join(result.Errors, "; ")
		if result.Suggestion != "" {
			if note != "" {
				note += "; "
			}
			note += result.Suggestion
		}
		if note != "" {
			notes[result.TaskID] = note
		}
	}
	return out, notes, nil
}
