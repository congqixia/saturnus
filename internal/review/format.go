package review

import (
	"fmt"
	"sort"
	"strings"
)

const summaryMarker = "REVIEW SUMMARY:"

// WorkerLabel distinguishes a pending retry's previous worker from an active
// assignment. Empty worker ids on running/completed records mean local execution.
func WorkerLabel(status, workerID string) string {
	if status == "pending" {
		if workerID != "" {
			return "unassigned (previous: " + workerID + ")"
		}
		return "unassigned"
	}
	if workerID == "" {
		return "server (local)"
	}
	return workerID
}

// StripANSI removes ANSI escape sequences from output (exported for remote
// workers that stream tool output to the server).
func StripANSI(s string) string {
	return stripANSI(s)
}

func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ExtractSummary returns the text after the last "REVIEW SUMMARY:" marker,
// or "" when the marker is absent.
func ExtractSummary(text string) string {
	idx := strings.LastIndex(text, summaryMarker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(text[idx+len(summaryMarker):])
}

type reviewSections struct {
	PRSummary   string
	Issues      string
	Suggestions string
}

func (s reviewSections) empty() bool {
	return s.PRSummary == "" && s.Issues == "" && s.Suggestions == ""
}

var reviewSectionLabels = []string{"PR summary:", "Issues:", "Review suggestions:"}

func parseReviewSections(summary string) reviewSections {
	var secs reviewSections
	lower := strings.ToLower(summary)
	type hit struct {
		pos   int
		label string
		index int
	}
	var hits []hit
	for i, label := range reviewSectionLabels {
		if pos := strings.Index(lower, strings.ToLower(label)); pos >= 0 {
			hits = append(hits, hit{pos: pos, label: label, index: i})
		}
	}
	if len(hits) == 0 {
		return secs
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	for i, h := range hits {
		end := len(summary)
		if i+1 < len(hits) {
			end = hits[i+1].pos
		}
		body := strings.TrimSpace(summary[h.pos+len(h.label) : end])
		switch h.index {
		case 0:
			secs.PRSummary = body
		case 1:
			secs.Issues = body
		case 2:
			secs.Suggestions = body
		}
	}
	return secs
}

func formatReviewSections(secs reviewSections) string {
	var b strings.Builder
	b.WriteString(summaryMarker)
	if secs.PRSummary != "" {
		fmt.Fprintf(&b, "\nPR summary:\n%s", truncate(secs.PRSummary, 1000))
	}
	if secs.Issues != "" {
		fmt.Fprintf(&b, "\n\nIssues:\n%s", truncate(secs.Issues, 2000))
	}
	if secs.Suggestions != "" {
		fmt.Fprintf(&b, "\n\nReview suggestions:\n%s", truncate(secs.Suggestions, 1000))
	}
	return b.String()
}
