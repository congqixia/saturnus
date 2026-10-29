package main

import (
	"fmt"
	"strings"

	"saturnus/internal/store"
)

func formatReviewFindings(findings []store.ReviewFinding, runs []store.ReviewRun) string {
	if len(findings) == 0 {
		return ""
	}
	seqs := make(map[string]int, len(runs))
	for _, run := range runs {
		seqs[run.ID] = run.Seq
	}
	var b strings.Builder
	b.WriteString("**Findings**:\n\n")
	b.WriteString("Run is the review round that created or last updated the finding; see Runs below.\n\n")
	b.WriteString("| # | Severity | Status | Run (last update) | Location | Finding | Fix |\n")
	b.WriteString("| - | -------- | ------ | ----------------- | -------- | ------- | --- |\n")
	for i, f := range findings {
		round := f.RunID
		if seq := seqs[f.RunID]; f.RunID != "" && seq > 0 {
			round = fmt.Sprintf("#%d", seq)
		}
		status := f.Status
		if status == "" {
			status = "open"
		}
		location := f.Path
		if f.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, f.Line)
		}
		detail := truncateText(f.Title, 200)
		if detail == "" {
			detail = truncateText(f.Message, 600)
		} else if f.Message != "" && f.Message != f.Title {
			detail += ": " + truncateText(f.Message, 600)
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s |\n", i+1,
			findingCell(f.Severity), findingCell(status), findingCell(round), findingCell(location),
			findingCell(detail), findingCell(truncateText(f.Suggestion, 300)))
	}
	return b.String()
}

func findingCell(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	value = strings.ReplaceAll(value, "\r", " ")
	return markdownTableCell(escapeMarkdown(value))
}
