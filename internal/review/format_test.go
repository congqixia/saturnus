package review

import (
	"strings"
	"testing"
)

func TestStripANSI(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "plain", want: "plain"},
		{in: "\x1b[0mhello\x1b[0m", want: "hello"},
		{in: "$ \x1b[0m\x1b[36mgh\x1b[0m pr view", want: "$ gh pr view"},
		{in: "\x1b[2K\x1b[1A\r\x1b[0Kprogress", want: "\rprogress"},
	}
	for _, tc := range cases {
		if got := stripANSI(tc.in); got != tc.want {
			t.Fatalf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractSummary(t *testing.T) {
	if got := extractSummary("no marker here"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	in := "some output\nREVIEW SUMMARY: LGTM overall, one nit about error handling.\n"
	if got := extractSummary(in); got != "LGTM overall, one nit about error handling." {
		t.Fatalf("extractSummary = %q", got)
	}
	in2 := "first\nREVIEW SUMMARY: old\nmore\nREVIEW SUMMARY: final verdict"
	if got := extractSummary(in2); got != "final verdict" {
		t.Fatalf("expected last summary, got %q", got)
	}
}

func TestParseReviewSectionsMultiline(t *testing.T) {
	in := `PR summary: Aborts load-backup on a MultiSave failure instead of racing on a shared error.
Issues:
- etcd_restore.go:40 shared err variable written by multiple goroutines
- etcd_restore.go:55 MultiSave failure silently swallowed
Review suggestions: Approve after adding a -race regression test.`
	secs := parseReviewSections(in)
	if secs.PRSummary != "Aborts load-backup on a MultiSave failure instead of racing on a shared error." {
		t.Fatalf("PRSummary = %q", secs.PRSummary)
	}
	if !strings.Contains(secs.Issues, "etcd_restore.go:40") || !strings.Contains(secs.Issues, "etcd_restore.go:55") {
		t.Fatalf("Issues = %q", secs.Issues)
	}
	if secs.Suggestions != "Approve after adding a -race regression test." {
		t.Fatalf("Suggestions = %q", secs.Suggestions)
	}
}

func TestParseReviewSectionsInline(t *testing.T) {
	in := "PR summary: fix the race. Issues: None. Review suggestions: LGTM"
	secs := parseReviewSections(in)
	if secs.PRSummary != "fix the race." || secs.Issues != "None." || secs.Suggestions != "LGTM" {
		t.Fatalf("unexpected sections: %#v", secs)
	}
}

func TestParseReviewSectionsFallback(t *testing.T) {
	if secs := parseReviewSections("no headers here"); !secs.empty() {
		t.Fatalf("expected empty sections, got %#v", secs)
	}
	if secs := parseReviewSections("PR summary: only this"); secs.empty() || secs.PRSummary != "only this" {
		t.Fatalf("unexpected sections: %#v", secs)
	}
}

func TestFormatReviewSections(t *testing.T) {
	secs := reviewSections{PRSummary: "fix the race", Issues: "shared err", Suggestions: "LGTM after fix"}
	got := formatReviewSections(secs)
	for _, want := range []string{"REVIEW SUMMARY:", "PR summary:", "Issues:", "Review suggestions:", "fix the race", "shared err", "LGTM after fix"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatReviewSections missing %q:\n%s", want, got)
		}
	}
}
