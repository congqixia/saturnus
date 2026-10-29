package main

import (
	"fmt"
	"strings"

	"saturnus/internal/lark"
	"saturnus/internal/review"
)

func (s *server) reviewFindingsReply(id string, page int) botReply {
	req, err := s.reviews.Get(id)
	if err != nil {
		return botReply{text: "Review not found."}
	}
	findings := s.reviews.ListFindings(req.ID)
	items, pages, err := review.FindingPage(findings, page)
	if err != nil {
		return botReply{text: err.Error()}
	}
	runs := s.reviews.ListRuns(req.ID)
	text, _ := review.FormatFindingPage(req, findings, runs, page)
	if len(items) == 0 {
		return botReply{text: text}
	}
	rows := make([]map[string]any, 0, len(items))
	for i, f := range items {
		status := f.Status
		if status == "" {
			status = "open"
		}
		location := review.FindingText(f.Path, 200)
		if f.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, f.Line)
		}
		detail := review.FindingText(f.Title, 200)
		if f.Message != "" && f.Message != f.Title {
			detail += "\n" + review.FindingText(f.Message, 600)
		}
		rows = append(rows, map[string]any{
			"number": (page-1)*review.FindingPageSize + i + 1, "severity": review.FindingText(f.Severity, 10),
			"status": review.FindingText(status, 30), "run": review.FindingRun(f, runs), "location": location,
			"finding": detail, "fix": review.FindingText(f.Suggestion, 300),
		})
	}
	description := fmt.Sprintf("%s\nPage %d/%d. Run = round that last updated the finding.", review.FindingSummary(findings), page, pages)
	if req.Verdict == "" {
		description += "\nPrevious findings; the current run has no verdict."
	}
	if page < pages {
		description += fmt.Sprintf("\nNext: `/sat findings %s %d`", req.ID, page+1)
	}
	return botReply{text: text, table: &lark.TableCard{
		Title: "Findings / " + req.ID, Description: description,
		Columns: []lark.TableColumn{
			{Name: "number", Display: "#", DataType: lark.ColumnNumber, Width: "5%"},
			{Name: "severity", Display: "Severity", DataType: lark.ColumnText, Width: "7%"},
			{Name: "status", Display: "Status", DataType: lark.ColumnText, Width: "10%"},
			{Name: "run", Display: "Run (last update)", DataType: lark.ColumnText, Width: "10%"},
			{Name: "location", Display: "Location", DataType: lark.ColumnText, Width: "18%"},
			{Name: "finding", Display: "Finding", DataType: lark.ColumnText, Width: "30%"},
			{Name: "fix", Display: "Fix", DataType: lark.ColumnText, Width: "20%"},
		}, Rows: rows,
	}}
}

func findingCell(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	value = strings.ReplaceAll(value, "\r", " ")
	return markdownTableCell(escapeMarkdown(value))
}
