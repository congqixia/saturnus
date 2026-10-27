package workermonitor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"saturnus/internal/ghinfo"
	"saturnus/internal/store"
)

type fakeNotifier struct {
	err      error
	disabled bool
	messages []string
	types    []string
	ids      []string
}

func (f *fakeNotifier) Enabled() bool { return !f.disabled }
func (f *fakeNotifier) SendText(typ, id, text string) error {
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, text)
	f.types = append(f.types, typ)
	f.ids = append(f.ids, id)
	return nil
}

var testNow = time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)

func register(t *testing.T, st *store.Store, id string, expiry *time.Time, refreshable bool, refreshExpiry *time.Time) {
	t.Helper()
	_, err := st.RegisterWorker(store.Agent{ID: id, Host: "worker-host"}, store.WorkerCapabilities{
		GH: true, GitHub: &ghinfo.Info{Version: "2.86.0", Host: ghinfo.Host, CheckedAt: testNow, TokenExpiresAt: expiry, Refreshable: refreshable, RefreshTokenExpiresAt: refreshExpiry},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func expiryAfter(d time.Duration) *time.Time { v := testNow.Add(d); return &v }

func TestWarningWindowAndRefreshableTokens(t *testing.T) {
	for _, tc := range []struct {
		name          string
		expiry        *time.Time
		refresh       bool
		refreshExpiry *time.Time
		want          int
		text          string
	}{
		{"unknown", nil, false, nil, 0, ""},
		{"exactly seven days", expiryAfter(7 * 24 * time.Hour), false, nil, 0, ""},
		{"just under seven days", expiryAfter(7*24*time.Hour - time.Second), false, nil, 1, "不足 7 天"},
		{"already expired", expiryAfter(-time.Second), false, nil, 1, "已过期"},
		{"auto refreshed access token", expiryAfter(time.Hour), true, expiryAfter(30 * 24 * time.Hour), 0, ""},
		{"refresh expires soon", expiryAfter(time.Hour), true, expiryAfter(6 * 24 * time.Hour), 1, "refresh token"},
		{"unknown refresh expiry", expiryAfter(time.Hour), true, nil, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			register(t, st, "worker", tc.expiry, tc.refresh, tc.refreshExpiry)
			sender := &fakeNotifier{}
			monitor := New(st, sender, Config{ReviewerOpenID: "ou_reviewer", ReviewerChatID: "oc_fallback"})
			if err := monitor.Check(testNow); err != nil {
				t.Fatal(err)
			}
			if len(sender.messages) != tc.want {
				t.Fatalf("messages=%v, want %d", sender.messages, tc.want)
			}
			if tc.want > 0 && (!strings.Contains(sender.messages[0], tc.text) || sender.ids[0] != "ou_reviewer" || sender.types[0] != "open_id") {
				t.Fatalf("wrong notification: %+v", sender)
			}
		})
	}
}

func TestDailyDedupSurvivesRestartAndExpiryRemindsImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	register(t, st, "worker", expiryAfter(48*time.Hour), false, nil)
	sender := &fakeNotifier{}
	m := New(st, sender, Config{ReviewerOpenID: "reviewer"})
	if err := m.Check(testNow); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(testNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("duplicate notification: %v", sender.messages)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m = New(st, sender, Config{ReviewerOpenID: "reviewer"})
	if err := m.Check(testNow.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 {
		t.Fatal("restart reset notification deduplication")
	}
	if err := m.Check(testNow.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 2 {
		t.Fatal("daily reminder missing")
	}
	if err := m.Check(testNow.Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 3 || !strings.Contains(sender.messages[2], "已过期") {
		t.Fatal("expiry transition did not remind immediately")
	}
}

func TestSendFailureAndMissingRecipientDoNotMarkDelivered(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	register(t, st, "worker", expiryAfter(time.Hour), false, nil)
	sender := &fakeNotifier{err: errors.New("temporary Lark failure")}
	m := New(st, sender, Config{ReviewerChatID: "oc_reviewer"})
	if err := m.Check(testNow); err == nil {
		t.Fatal("send error was ignored")
	}
	sender.err = nil
	missing := New(st, sender, Config{})
	if err := missing.Check(testNow); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(testNow); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 || sender.types[0] != "chat_id" || sender.ids[0] != "oc_reviewer" {
		t.Fatalf("recovery failed: %+v", sender)
	}
}

func TestRenewedTokenStopsReminderAndLegacyWorkerIsIgnored(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	register(t, st, "worker", expiryAfter(time.Hour), false, nil)
	if _, err := st.RegisterWorker(store.Agent{ID: "legacy"}, store.WorkerCapabilities{GH: true}); err != nil {
		t.Fatal(err)
	}
	sender := &fakeNotifier{}
	m := New(st, sender, Config{ReviewerOpenID: "reviewer"})
	if err := m.Check(testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateWorkerGitHub("worker", &ghinfo.Info{Version: "2.86.0", Host: ghinfo.Host, CheckedAt: testNow.Add(time.Hour), TokenExpiresAt: expiryAfter(30 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(testNow.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("renewed token still warned: %v", sender.messages)
	}
	// Cancellation joins the monitor without leaking its ticker goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop := m.Start(ctx)
	stop()
}

func TestStorageFailureIsReported(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := New(st, &fakeNotifier{}, Config{}).Check(testNow); err == nil {
		t.Fatal("database failure silently treated as no expiring workers")
	}
}
