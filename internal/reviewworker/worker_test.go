package reviewworker

import (
	"testing"
	"time"

	"saturnus/internal/ghinfo"
)

func TestHeartbeatRefreshesGitHubMetadataAndPreservesExpiryOnFailure(t *testing.T) {
	expiry := time.Now().UTC().Add(6 * 24 * time.Hour)
	initial := &ghinfo.Info{Version: "2.86.0", TokenExpiresAt: &expiry, ExpiryStatus: "known", ExpirySource: "github_response_header"}
	w := New(Config{GH: true, GitHub: initial})
	if got := w.heartbeat().GitHub; got == nil || !got.TokenExpiresAt.Equal(expiry) {
		t.Fatalf("initial metadata missing: %+v", got)
	}
	failure := &ghinfo.Info{Version: "2.86.0", ExpiryStatus: "check_failed"}
	w.updateGitHub(failure)
	if got := w.heartbeat().GitHub; got.ExpiryStatus != "check_failed" || !got.TokenExpiresAt.Equal(expiry) {
		t.Fatalf("probe failure erased known expiry: %+v", got)
	}
	renewed := expiry.Add(30 * 24 * time.Hour)
	w.updateGitHub(&ghinfo.Info{Version: "2.87.0", TokenExpiresAt: &renewed, ExpiryStatus: "known"})
	if got := w.heartbeat().GitHub; got.Version != "2.87.0" || !got.TokenExpiresAt.Equal(renewed) {
		t.Fatalf("renewal was not reported: %+v", got)
	}
	if !initial.TokenExpiresAt.Equal(expiry) {
		t.Fatal("new probe mutated an old heartbeat snapshot")
	}
}
