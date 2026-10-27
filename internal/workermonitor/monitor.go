// Package workermonitor warns the reviewer about expiring worker credentials.
package workermonitor

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"saturnus/internal/store"
)

const warningWindow = 7 * 24 * time.Hour
const repeatAfter = 24 * time.Hour

type notifier interface {
	Enabled() bool
	SendText(receiveIDType, receiveID, text string) error
}

type Config struct {
	ReviewerOpenID string
	ReviewerChatID string
	CheckInterval  time.Duration
}

type Monitor struct {
	st     *store.Store
	sender notifier
	cfg    Config
	wake   chan struct{}
	mu     sync.Mutex
}

func New(st *store.Store, sender notifier, cfg Config) *Monitor {
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = time.Hour
	}
	return &Monitor{st: st, sender: sender, cfg: cfg, wake: make(chan struct{}, 1)}
}

// Notify coalesces registration/heartbeat changes without blocking scheduling.
func (m *Monitor) Notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Monitor) Start(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(m.cfg.CheckInterval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			if err := m.check(ctx, time.Now().UTC()); err != nil {
				log.Printf("worker token expiry check: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-m.wake:
			}
		}
	}()
	return func() { cancel(); <-done }
}

// Check sends at most one reminder per worker, credential expiry and day. An
// expired credential produces a separate immediate reminder. Short-lived access
// tokens that gh refreshes automatically use the refresh-token expiry instead.
func (m *Monitor) Check(now time.Time) error {
	return m.check(context.Background(), now)
}

func (m *Monitor) check(ctx context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	workers, err := m.st.ListWorkersWithError()
	if err != nil {
		return err
	}
	var firstErr error
	for _, agent := range workers {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		caps, err := agent.CapabilitiesDecoded()
		if err != nil || !caps.GH || caps.GitHub == nil {
			continue
		}
		info := caps.GitHub
		expiry, kind := info.TokenExpiresAt, "access token"
		if info.Refreshable {
			expiry, kind = info.RefreshTokenExpiresAt, "refresh token"
		}
		if expiry == nil || expiry.IsZero() || expiry.Sub(now) >= warningWindow {
			continue
		}
		phase := "expiring"
		remaining := fmt.Sprintf("剩余约 %d 天（不足 7 天）", int(math.Ceil(expiry.Sub(now).Hours()/24)))
		if !expiry.After(now) {
			phase, remaining = "expired", "已过期"
		}
		key := fmt.Sprintf("%s|%s|%s|%s", info.Host, kind, expiry.UTC().Format(time.RFC3339Nano), phase)
		last, err := m.st.WorkerTokenAlertSentAt(agent.ID, key)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !last.IsZero() && now.Sub(last) < repeatAfter {
			continue
		}
		typ, recipient := "open_id", m.cfg.ReviewerOpenID
		if recipient == "" {
			typ, recipient = "chat_id", m.cfg.ReviewerChatID
		}
		if m.sender == nil || !m.sender.Enabled() || recipient == "" {
			log.Printf("worker %s GitHub %s %s; configure Lark and review reviewer to receive reminders", agent.ID, kind, remaining)
			continue
		}
		message := fmt.Sprintf("Saturnus worker GitHub 凭据到期提醒\nworker=%s\nhost=%s\ngh=%s\nGitHub=%s\n凭据=%s\n到期时间=%s\n%s\n请在 worker 服务实际使用的用户和环境下更新凭据；worker 将自动重新检查并上报。", agent.ID, agent.Host, info.Version, info.Host, kind, expiry.UTC().Format(time.RFC3339), remaining)
		if err := m.sender.SendText(typ, recipient, message); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("notify reviewer for worker %s: %w", agent.ID, err)
			}
			continue
		}
		if err := m.st.MarkWorkerTokenAlertSent(agent.ID, key, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
