package review

import "testing"

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
