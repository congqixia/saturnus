package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"saturnus/internal/store"
	"saturnus/internal/tasks"
)

// Task detail cards reserve space for the card envelope and JSON escaping.
// Pagination keeps every association addressable even for very large tasks.
const taskDetailPageSize = 3
const taskDetailEncodedBudget = 18000

func taskDetailText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max]) + "…"
	}
	return escapeMarkdown(s)
}

func taskDetailCode(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		if r == '`' {
			return '\''
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func taskDetailLink(label, raw string) string {
	label = taskDetailText(label, 100)
	if len(raw) > 1024 || strings.ContainsAny(raw, "\\ \r\n\t<>") {
		return label
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return label
	}
	return "[" + label + "](" + strings.NewReplacer("(", "%28", ")", "%29").Replace(raw) + ")"
}

func formatBoundedTaskDetail(task store.Task, docs []store.TaskDoc, prs []store.TaskPR, sessions []store.TaskSession, refs []store.TaskRef, page int) (string, int) {
	maxItems := max(len(docs), len(prs), len(sessions), len(refs))
	pages := max(1, (maxItems+taskDetailPageSize-1)/taskDetailPageSize)
	if page < 1 || page > pages {
		return "", pages
	}
	start := (page - 1) * taskDetailPageSize
	var b strings.Builder
	// Reserve additional space for the omitted-item notice and JSON card envelope.
	appendText := func(s string) bool {
		first, _ := json.Marshal(b.String() + s)
		second, _ := json.Marshal(string(first))
		if len(second) > taskDetailEncodedBudget-500 {
			return false
		}
		b.WriteString(s)
		return true
	}
	appendText(fmt.Sprintf("**Title**: %s\n**Status**: %s\n", taskDetailText(task.Title, 200), taskDetailText(task.Status, 32)))
	if task.IssueURL != "" {
		appendText(fmt.Sprintf("**Issue**: %s (%s)\n", taskDetailLink(issueRef(task), task.IssueURL), taskDetailText(task.IssueState, 32)))
	}
	if task.Description != "" {
		appendText(fmt.Sprintf("**Description**: %s\n", taskDetailText(task.Description, 600)))
	}
	if !task.CreatedAt.IsZero() {
		appendText("**Created**: " + task.CreatedAt.Local().Format("2006-01-02 15:04:05") + "\n")
	}
	if !task.UpdatedAt.IsZero() {
		appendText("**Updated**: " + task.UpdatedAt.Local().Format("2006-01-02 15:04:05") + "\n")
	}
	if task.RefreshedAt.IsZero() {
		appendText("**Last successful refresh**: never\n")
	} else {
		appendText("**Last successful refresh**: " + task.RefreshedAt.Local().Format("2006-01-02 15:04:05") + "\n")
	}
	if task.RefreshError != "" {
		appendText("**Refresh error**: " + taskDetailText(task.RefreshError, 300) + "\n")
	}
	if suggestion := tasks.CloseSuggestion(task, prs); suggestion != "" {
		appendText("**Suggestion**: " + taskDetailText(suggestion, 200) + "\n")
	}
	appendText(fmt.Sprintf("\nPage %d/%d · up to %d items per section\n", page, pages, taskDetailPageSize))
	omitted := false
	section := func(name string, count int, render func(int) string) {
		if start >= count {
			return
		}
		if !appendText(fmt.Sprintf("\n**%s** (%d)\n", name, count)) {
			omitted = true
			return
		}
		for i := start; i < min(start+taskDetailPageSize, count); i++ {
			if !appendText(render(i)) {
				omitted = true
			}
		}
	}
	section("Refs", len(refs), func(i int) string {
		ref := refs[i]
		label := ref.Title
		if label == "" {
			label = ref.URL
		}
		return fmt.Sprintf("- `%s` %s\n", taskDetailCode(ref.ID, 80), taskDetailLink(label, ref.URL))
	})
	section("Sessions", len(sessions), func(i int) string {
		v := sessions[i]
		return fmt.Sprintf("- `%s` %s: %s · %s %s\n", taskDetailCode(v.ID, 80), taskDetailText(v.Tool, 40), taskDetailText(v.SessionID, 120), taskDetailText(v.Purpose, 100), taskDetailText(v.Note, 100))
	})
	section("Docs", len(docs), func(i int) string {
		v := docs[i]
		return fmt.Sprintf("- `%s` %s:%s\n", taskDetailCode(v.ID, 80), taskDetailText(v.Repo, 100), taskDetailText(v.Path, 200))
	})
	section("PRs", len(prs), func(i int) string {
		v := prs[i]
		label := fmt.Sprintf("%s#%d", v.Repo, v.PRNumber)
		fsTask := ""
		if v.FsTaskURL != "" {
			fsTask = " · " + taskDetailLink("Feishu task", v.FsTaskURL)
		}
		return fmt.Sprintf("- %s · %s · review %s%s\n  %s\n", taskDetailLink(label, v.PRURL), taskDetailText(v.PRState, 30), taskDetailText(v.ReviewStatus, 30), fsTask, taskDetailText(v.PRTitle, 160))
	})
	if omitted {
		b.WriteString("\nSome items exceeded the card size budget; use the Task API for full details.\n")
	}
	if pages > 1 {
		fmt.Fprintf(&b, "\nUse `/sat task %s <page>` to view more.", taskDetailCode(task.ID, 80))
	}
	return strings.TrimSpace(b.String()), pages
}
