package review

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"saturnus/internal/store"
)

// FindingPageSize bounds both command replies and the second reviewer message.
const FindingPageSize = 5

func FindingSummary(findings []store.ReviewFinding) string {
	severity, status := map[string]int{}, map[string]int{}
	for _, f := range findings {
		severity[f.Severity]++
		s := f.Status
		if s == "" {
			s = "open"
		}
		status[s]++
	}
	return fmt.Sprintf("Findings: %d (P0: %d, P1: %d, P2: %d)\nStatus: open=%d addressed=%d commented=%d resolved=%d",
		len(findings), severity["P0"], severity["P1"], severity["P2"], status["open"], status["addressed"], status["commented"], status["resolved"])
}

func FindingPage(findings []store.ReviewFinding, page int) ([]store.ReviewFinding, int, error) {
	pages := max(1, (len(findings)+FindingPageSize-1)/FindingPageSize)
	if page < 1 || page > pages {
		return nil, pages, fmt.Errorf("finding page must be between 1 and %d", pages)
	}
	start := (page - 1) * FindingPageSize
	return findings[start:min(start+FindingPageSize, len(findings))], pages, nil
}

// FindingText limits bytes without breaking UTF-8, including when the content
// contains multi-byte filenames or messages. It does not alter stored findings.
func FindingText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func FindingRun(f store.ReviewFinding, runs []store.ReviewRun) string {
	for _, run := range runs {
		if f.RunID != "" && run.ID == f.RunID && run.Seq > 0 {
			return fmt.Sprintf("#%d", run.Seq)
		}
	}
	if f.RunID != "" {
		return FindingText(f.RunID, 100)
	}
	return "-"
}

func FormatFindingPage(req store.ReviewRequest, findings []store.ReviewFinding, runs []store.ReviewRun, page int) (string, error) {
	items, pages, err := FindingPage(findings, page)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Review %s (%s#%d)\n%s\nPage %d/%d; run = round that last updated the finding.\n", req.ID, req.Repo, req.PRNumber, FindingSummary(findings), page, pages)
	if req.Verdict == "" && len(findings) > 0 {
		b.WriteString("Previous findings; the current run has no verdict.\n")
	}
	if len(items) == 0 {
		b.WriteString("No findings.\n")
	}
	for i, f := range items {
		status := f.Status
		if status == "" {
			status = "open"
		}
		fmt.Fprintf(&b, "\n%d. [%s] %s\nID: %s; Status: %s; Run (last update): %s\nLocation: %s:%d\n",
			(page-1)*FindingPageSize+i+1, FindingText(f.Severity, 10), FindingText(f.Title, 200), FindingText(f.ID, 100), FindingText(status, 30), FindingRun(f, runs), FindingText(f.Path, 200), f.Line)
		if f.Message != "" {
			fmt.Fprintf(&b, "%s\n", FindingText(f.Message, 600))
		}
		if f.Suggestion != "" {
			fmt.Fprintf(&b, "Fix: %s\n", FindingText(f.Suggestion, 300))
		}
	}
	fmt.Fprintf(&b, "\nDetails: /sat findings %s %d", req.ID, page)
	if page < pages {
		fmt.Fprintf(&b, "\nNext: /sat findings %s %d", req.ID, page+1)
	}
	return b.String(), nil
}
