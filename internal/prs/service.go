package prs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"saturnus/internal/keylock"
	"saturnus/internal/store"
)

type Config struct {
	Enabled         bool
	Context         context.Context
	GHCli           string
	Repos           []string
	Author          string
	RefreshInterval time.Duration
	RefreshTimeout  time.Duration
	Policy          Policy
}

type Detail struct {
	PR          store.PullRequest `json:"pr"`
	Checks      []store.PRCheck   `json:"checks"`
	ReviewID    string            `json:"review_id,omitempty"`
	TaskIDs     []string          `json:"task_ids"`
	ChecksStale bool              `json:"checks_stale"`
}

type SweepResult struct {
	Refreshed int      `json:"refreshed"`
	Errors    []string `json:"errors"`
}

type Service struct {
	st     *store.Store
	cfg    Config
	github Provider
	repos  map[string]bool
	locks  keylock.Map
}

func New(st *store.Store, cfg Config) *Service {
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	if cfg.Author == "" {
		cfg.Author = "@me"
	}
	if cfg.RefreshTimeout <= 0 {
		cfg.RefreshTimeout = 2 * time.Minute
	}
	s := &Service{st: st, cfg: cfg, github: GitHub{CLI: cfg.GHCli}, repos: map[string]bool{}}
	for _, repo := range cfg.Repos {
		s.repos[strings.ToLower(repo)] = true
	}
	return s
}

func (s *Service) Enabled() bool { return s != nil && s.cfg.Enabled }

func (s *Service) allowed(repo string) error {
	if !s.Enabled() {
		return fmt.Errorf("PR monitor is disabled")
	}
	if !ValidRepo(repo) || !s.repos[strings.ToLower(repo)] {
		return fmt.Errorf("repo is not in the PR whitelist: %s", repo)
	}
	return nil
}

func ParseRef(ref string) (string, int, error) {
	repo, num, ok := strings.Cut(ref, "#")
	if !ok {
		u, err := url.Parse(ref)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || (u.Host != "github.com" && u.Host != "www.github.com") {
			return "", 0, fmt.Errorf("use a GitHub PR URL or owner/repo#number")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 4 || parts[2] != "pull" {
			return "", 0, fmt.Errorf("invalid GitHub PR URL")
		}
		repo, num = parts[0]+"/"+parts[1], parts[3]
	}
	n, err := strconv.Atoi(num)
	if !ValidRepo(repo) || err != nil || n <= 0 {
		return "", 0, fmt.Errorf("invalid PR reference")
	}
	return strings.ToLower(repo), n, nil
}

func (s *Service) snapshot(ctx context.Context, pr store.PullRequest) (Detail, error) {
	if err := s.allowed(pr.Repo); err != nil {
		return Detail{}, err
	}
	checks, err := s.st.PullRequestChecks(ctx, pr.ID)
	if err != nil {
		return Detail{}, err
	}
	pr.CIState, pr.Counts = s.cfg.Policy.Evaluate(pr.Repo, checks)
	stale := pr.SyncError != "" || pr.CheckedHeadSHA == "" || pr.CheckedHeadSHA != pr.HeadSHA || pr.CheckedMergeSHA != pr.MergeSHA
	if stale {
		pr.CIState = "UNKNOWN"
	}
	tasks, err := s.st.PullRequestTaskIDs(ctx, pr.Repo, pr.Number)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{PR: pr, Checks: checks, TaskIDs: tasks, ChecksStale: stale}
	d.ReviewID, err = s.st.PullRequestReviewID(ctx, pr.Repo, pr.Number)
	if err != nil {
		return Detail{}, err
	}
	return d, nil
}

func (s *Service) Get(ctx context.Context, id string) (Detail, error) {
	pr, err := s.st.GetPullRequest(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	return s.snapshot(ctx, pr)
}

func (s *Service) List(ctx context.Context, all bool) ([]store.PullRequest, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("PR monitor is disabled")
	}
	rows, err := s.st.ListPullRequests(ctx, all)
	if err != nil {
		return nil, err
	}
	out := []store.PullRequest{}
	for _, pr := range rows {
		if !s.repos[pr.Repo] {
			continue
		}
		d, err := s.snapshot(ctx, pr)
		if err != nil {
			return nil, err
		}
		out = append(out, d.PR)
	}
	return out, nil
}

func (s *Service) resolve(ctx context.Context, ref string) (store.PullRequest, error) {
	if strings.HasPrefix(ref, "pr_") {
		pr, err := s.st.GetPullRequest(ctx, ref)
		if err != nil {
			return pr, err
		}
		return pr, s.allowed(pr.Repo)
	}
	repo, n, err := ParseRef(ref)
	if err != nil {
		return store.PullRequest{}, err
	}
	if err = s.allowed(repo); err != nil {
		return store.PullRequest{}, err
	}
	return s.st.TrackPullRequest(ctx, repo, n)
}

func (s *Service) Refresh(ctx context.Context, ref string) (Detail, error) {
	pr, err := s.resolve(ctx, ref)
	if err != nil {
		return Detail{}, err
	}
	return s.refresh(ctx, pr.ID)
}

func (s *Service) refresh(parent context.Context, id string) (Detail, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.cfg.Context, cancel)
	defer stop()
	unlock, err := s.locks.Lock(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	defer unlock()
	pr, err := s.st.GetPullRequest(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	if err = s.allowed(pr.Repo); err != nil {
		return Detail{}, err
	}
	pr.SyncAttemptedAt = time.Now().UTC()
	metadata, checks, fetchErr := s.github.Fetch(ctx, pr.Repo, pr.Number)
	if metadata.HeadSHA != "" {
		// Keep identity and the last good CI observation even if collection
		// failed after metadata was fetched (including a concurrent PR push).
		metadata.ID, metadata.SyncedAt, metadata.CheckedHeadSHA, metadata.CheckedMergeSHA = pr.ID, pr.SyncedAt, pr.CheckedHeadSHA, pr.CheckedMergeSHA
		metadata.Monitoring = pr.Monitoring
		metadata.SyncAttemptedAt = pr.SyncAttemptedAt
		if metadata.CreatedAt.IsZero() {
			metadata.CreatedAt = pr.CreatedAt
		}
		if metadata.UpdatedAt.IsZero() {
			metadata.UpdatedAt = pr.UpdatedAt
		}
		if metadata.URL == "" {
			metadata.URL = pr.URL
		}
		pr = metadata
	}
	pr.SyncError = ""
	if fetchErr != nil {
		pr.CIState = "UNKNOWN"
		pr.SyncError = fetchErr.Error()
	} else {
		pr.CIState, pr.Counts = s.cfg.Policy.Evaluate(pr.Repo, checks)
		pr.CheckedHeadSHA, pr.CheckedMergeSHA = pr.HeadSHA, pr.MergeSHA
		pr.SyncedAt = time.Now().UTC()
	}
	checkpoint, end := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer end()
	if err = s.st.SavePullRequestSnapshot(checkpoint, pr, checks, fetchErr == nil); err != nil {
		return Detail{}, errors.Join(fetchErr, err)
	}
	d, err := s.snapshot(checkpoint, pr)
	return d, errors.Join(fetchErr, err)
}

func (s *Service) Sweep(parent context.Context) (SweepResult, error) {
	result := SweepResult{Errors: []string{}}
	if !s.Enabled() {
		return result, fmt.Errorf("PR monitor is disabled")
	}
	ctx, cancel := context.WithTimeout(parent, s.cfg.RefreshTimeout)
	defer cancel()
	stop := context.AfterFunc(s.cfg.Context, cancel)
	defer stop()
	unlock, err := s.locks.Lock(ctx, "sweep")
	if err != nil {
		return result, err
	}
	defer unlock()
	author := s.cfg.Author
	if author == "@me" {
		author, err = s.github.CurrentUser(ctx)
		if err != nil {
			result.Errors = append(result.Errors, "discovery identity: "+err.Error())
		}
	}
	force := map[string]bool{}
	if author != "" && err == nil {
		repos := make([]string, 0, len(s.repos))
		for repo := range s.repos {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			if ctx.Err() != nil {
				break
			}
			numbers, err := s.github.Discover(ctx, repo, author)
			if err != nil {
				result.Errors = append(result.Errors, repo+" discovery: "+err.Error())
				continue
			}
			for _, n := range numbers {
				pr, err := s.st.TrackPullRequest(ctx, repo, n)
				if err != nil {
					return result, err
				}
				force[pr.ID] = true
			}
		}
	}
	seeds, err := s.st.PullRequestSeeds(ctx)
	if err != nil {
		return result, err
	}
	for _, ref := range seeds {
		if s.repos[strings.ToLower(ref.Repo)] {
			if _, err = s.st.TrackPullRequest(ctx, ref.Repo, ref.Number); err != nil {
				return result, err
			}
		}
	}
	rows, err := s.st.ListPullRequests(ctx, true)
	if err != nil {
		return result, err
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].SyncAttemptedAt.Before(rows[j].SyncAttemptedAt) })
	jobs := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				d, err := s.refresh(ctx, id)
				mu.Lock()
				if err != nil {
					result.Errors = append(result.Errors, id+": "+err.Error())
				} else if d.PR.ID != "" {
					result.Refreshed++
				}
				mu.Unlock()
			}
		}()
	}
loop:
	for _, pr := range rows {
		if !pr.Monitoring || !s.repos[pr.Repo] || (!force[pr.ID] && pr.State != "OPEN" && pr.State != "UNKNOWN") {
			continue
		}
		select {
		case jobs <- pr.ID:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	sort.Strings(result.Errors)
	return result, ctx.Err()
}

// SetMonitoring changes only local polling, never upstream checks or branch
// protection. Manual Refresh remains available while polling is paused.
func (s *Service) SetMonitoring(ctx context.Context, ref string, enabled bool) (Detail, error) {
	pr, err := s.resolve(ctx, ref)
	if err != nil {
		return Detail{}, err
	}
	unlock, err := s.locks.Lock(ctx, pr.ID)
	if err != nil {
		return Detail{}, err
	}
	defer unlock()
	pr, err = s.st.GetPullRequest(ctx, pr.ID)
	if err != nil {
		return Detail{}, err
	}
	pr.Monitoring = enabled
	if err = s.st.SavePullRequestSnapshot(ctx, pr, nil, false); err != nil {
		return Detail{}, err
	}
	return s.snapshot(ctx, pr)
}

func (s *Service) Start(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !s.Enabled() || s.cfg.RefreshInterval <= 0 || len(s.repos) == 0 {
			return
		}
		for ctx.Err() == nil {
			r, err := s.Sweep(ctx)
			log.Printf("PR monitor refreshed=%d errors=%v error=%v", r.Refreshed, r.Errors, err)
			timer := time.NewTimer(s.cfg.RefreshInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
