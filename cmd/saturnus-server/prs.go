package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/prs"
	"saturnus/internal/store"
)

func prsConfig(repos map[string]string) (prs.Config, error) {
	interval, err := strictDuration("SATURNUS_PRS_REFRESH_INTERVAL", 5*time.Minute, true)
	if err != nil {
		return prs.Config{}, err
	}
	timeout, err := strictDuration("SATURNUS_PRS_REFRESH_TIMEOUT", 2*time.Minute, false)
	if err != nil {
		return prs.Config{}, err
	}
	policy, err := prs.LoadPolicy(os.Getenv("SATURNUS_PRS_BYPASS_FILE"))
	if err != nil {
		return prs.Config{}, err
	}
	names := make([]string, 0, len(repos))
	for repo := range repos {
		if !prs.ValidRepo(repo) {
			return prs.Config{}, fmt.Errorf("invalid PR repo %q", repo)
		}
		names = append(names, repo)
	}
	sort.Strings(names)
	return prs.Config{Enabled: os.Getenv("SATURNUS_PRS_ENABLED") != "0", GHCli: os.Getenv("SATURNUS_PRS_GH_CLI"), Author: os.Getenv("SATURNUS_PRS_AUTHOR"),
		Repos: names, RefreshInterval: interval, RefreshTimeout: timeout, Policy: policy}, nil
}

func (s *server) prsReply() botReply {
	if s.prs == nil || !s.prs.Enabled() {
		return s.legacyPRsReply()
	}
	return s.prListReply(false, false)
}

func (s *server) prListReply(refresh, all bool) botReply {
	if s.prs == nil || !s.prs.Enabled() {
		return botReply{text: "PR monitor is disabled."}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rows, err := s.prs.List(ctx, all)
	if err != nil {
		return botReply{text: "Load PRs failed: " + err.Error()}
	}
	var notes []string
	if refresh || len(rows) == 0 {
		result, err := s.prs.Sweep(ctx)
		notes = append(notes, result.Errors...)
		if err != nil {
			notes = append(notes, err.Error())
		}
		readCtx, end := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		rows, err = s.prs.List(readCtx, all)
		end()
		if err != nil {
			return botReply{text: "Load PRs failed: " + err.Error()}
		}
	}
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString("No tracked open PRs. Use /sat pr <url> to track a PR.\n")
	}
	total := len(rows)
	if total > 10 {
		rows = rows[:10]
	}
	cells := make([]map[string]any, 0, len(rows))
	for _, pr := range rows {
		label, color := pr.CIState, prCIColor(pr.CIState)
		if !pr.Monitoring {
			label, color = "PAUSED / "+label, "grey"
		}
		fmt.Fprintf(&b, "%s %s#%d %s base=%s head=%s CI=%s bypass=%d checked=%s\n", pr.ID, pr.Repo, pr.Number, pr.Title, pr.BaseBranch, pr.HeadBranch, label, pr.Counts.Bypass, pr.SyncedAt.Format(time.RFC3339))
		if pr.SyncError != "" {
			fmt.Fprintf(&b, "  sync error: %s\n", pr.SyncError)
		}
		cells = append(cells, map[string]any{"repo": fmt.Sprintf("%s#%d", pr.Repo, pr.Number), "title": pr.Title, "base": pr.BaseBranch, "head": pr.HeadBranch,
			"draft": boolOption(pr.Draft), "ci": optionTag(label, color), "bypass": pr.Counts.Bypass, "checked": lark.UnixMillis(pr.SyncedAt)})
	}
	if total > len(rows) {
		fmt.Fprintf(&b, "Showing %d of %d tracked PRs.\n", len(rows), total)
	}
	if len(notes) > 0 {
		fmt.Fprintf(&b, "Refresh errors:\n%s", strings.Join(notes, "\n"))
	}
	title := "PRs / CI"
	if len(notes) > 0 {
		title += " (refresh errors)"
	}
	reply := botReply{text: strings.TrimSpace(b.String())}
	if len(cells) > 0 {
		reply.table = &lark.TableCard{Title: title, Columns: []lark.TableColumn{
			{Name: "repo", Display: "Repo#PR", DataType: lark.ColumnText, Width: "18%"},
			{Name: "title", Display: "Title", DataType: lark.ColumnText, Width: "24%"},
			{Name: "base", Display: "Base", DataType: lark.ColumnText, Width: "10%"},
			{Name: "head", Display: "Head", DataType: lark.ColumnText, Width: "10%"},
			{Name: "draft", Display: "Draft", DataType: lark.ColumnOption, Width: "6%"},
			{Name: "ci", Display: "CI", DataType: lark.ColumnOption, Width: "12%"},
			{Name: "bypass", Display: "Bypass", DataType: lark.ColumnNumber, Width: "6%"},
			{Name: "checked", Display: "CI checked", DataType: lark.ColumnDate, DateFormat: "YYYY-MM-DD HH:mm", Width: "14%"},
		}, Rows: cells}
		if total > len(rows) {
			reply.table.Description = fmt.Sprintf("Showing %d of %d tracked PRs.\n", len(rows), total)
		}
		if len(notes) > 0 {
			reply.table.Description += "Refresh errors:\n" + escapeMarkdown(limitPRText(strings.Join(notes, "\n"), 6000))
		}
	}
	return reply
}

func prCIColor(state string) string {
	switch state {
	case "PASS":
		return "green"
	case "FAIL":
		return "red"
	case "PENDING":
		return "orange"
	case "CANCEL":
		return "yellow"
	default:
		return "grey"
	}
}

func (s *server) prDetailReply(ref string) botReply {
	if s.prs == nil || !s.prs.Enabled() {
		return botReply{text: "PR monitor is disabled."}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	d, err := s.prs.Refresh(ctx, ref)
	if d.PR.ID == "" {
		if err == nil {
			err = store.ErrNotFound
		}
		return botReply{text: "PR lookup failed: " + err.Error()}
	}
	content := formatPRDetail(d)
	reply := botReply{text: content, md: &lark.MarkdownCard{Title: fmt.Sprintf("PR %s#%d", d.PR.Repo, d.PR.Number), Content: content}}
	if len(d.Checks) > 0 {
		reply.table = prCheckTable(d, content)
	}
	return reply
}

func prCheckTable(d prs.Detail, content string) *lark.TableCard {
	rows := make([]map[string]any, 0, len(d.Checks))
	for _, c := range d.Checks {
		state := c.Status
		if c.Conclusion != "" {
			state += " / " + c.Conclusion
		}
		bypass := "no"
		if c.Bypassed {
			bypass = "YES (" + c.BypassRule + ")"
		}
		sha := c.HeadSHA
		if len(sha) > 8 {
			sha = sha[:8]
		}
		rows = append(rows, map[string]any{"source": c.Source, "workflow": c.Workflow, "check": c.Name, "state": optionTag(state, prCIColor(strings.ToUpper(c.Bucket))), "bypass": bypass, "sha": sha, "details": c.Description + " " + c.URL})
	}
	header, _, _ := strings.Cut(content, "\n| Source |")
	return &lark.TableCard{Title: fmt.Sprintf("PR %s#%d / CI", d.PR.Repo, d.PR.Number), Description: header, Columns: []lark.TableColumn{
		{Name: "source", Display: "Source", DataType: lark.ColumnText, Width: "10%"},
		{Name: "workflow", Display: "Workflow", DataType: lark.ColumnText, Width: "14%"},
		{Name: "check", Display: "Check", DataType: lark.ColumnText, Width: "22%"},
		{Name: "state", Display: "Upstream result", DataType: lark.ColumnOption, Width: "18%"},
		{Name: "bypass", Display: "Bypass", DataType: lark.ColumnText, Width: "12%"},
		{Name: "sha", Display: "Tested SHA", DataType: lark.ColumnText, Width: "10%"},
		{Name: "details", Display: "Details", DataType: lark.ColumnText, Width: "14%"},
	}, Rows: rows}
}

func formatPRDetail(d prs.Detail) string {
	pr := d.PR
	var b strings.Builder
	fmt.Fprintf(&b, "**Monitoring**: %t\n", pr.Monitoring)
	fmt.Fprintf(&b, "**PR**: [%s#%d](%s)\n**ID**: `%s`\n**Title**: %s\n**State**: %s\n**Author**: %s\n**Base**: %s\n**Head branch**: %s\n**Head SHA**: `%s`\n**CI**: %s\n",
		pr.Repo, pr.Number, pr.URL, pr.ID, escapeMarkdown(pr.Title), pr.State, escapeMarkdown(pr.Author), escapeMarkdown(pr.BaseBranch), escapeMarkdown(pr.HeadBranch), pr.HeadSHA, pr.CIState)
	fmt.Fprintf(&b, "**Effective checks**: pass=%d fail=%d pending=%d cancel=%d skip=%d unknown=%d bypass=%d\n", pr.Counts.Pass, pr.Counts.Fail, pr.Counts.Pending, pr.Counts.Cancel, pr.Counts.Skip, pr.Counts.Unknown, pr.Counts.Bypass)
	if !pr.SyncedAt.IsZero() {
		fmt.Fprintf(&b, "**Last complete CI sync**: %s\n", pr.SyncedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if pr.SyncError != "" {
		fmt.Fprintf(&b, "**Sync error**: %s\nChecks below are the last complete snapshot, not a new CI result.\n", escapeMarkdown(pr.SyncError))
	}
	if pr.CheckedHeadSHA != "" {
		fmt.Fprintf(&b, "**Snapshot head SHA**: `%s`\n", pr.CheckedHeadSHA)
	}
	if pr.CheckedMergeSHA != "" {
		fmt.Fprintf(&b, "**Snapshot test-merge SHA**: `%s`\n", pr.CheckedMergeSHA)
	}
	if d.ReviewID != "" {
		fmt.Fprintf(&b, "**Review**: `%s`\n", d.ReviewID)
	}
	if len(d.TaskIDs) > 0 {
		fmt.Fprintf(&b, "**Tasks**: %s\n", strings.Join(d.TaskIDs, ", "))
	}
	if len(d.Checks) == 0 {
		if pr.SyncedAt.IsZero() {
			b.WriteString("\nNo complete CI snapshot yet.\n")
		} else {
			b.WriteString("\nNo reported checks in the complete snapshot.\n")
		}
		return b.String()
	}
	b.WriteString("\nBypassed items retain their upstream result and do not affect the CI summary.\n\n")
	b.WriteString("| Source | Workflow | Check | Result | Bypass | Tested SHA | Details |\n| ------ | -------- | ----- | ------ | ------ | ---------- | ------- |\n")
	for _, c := range d.Checks {
		bypass := "no"
		if c.Bypassed {
			bypass = "YES (" + c.BypassRule + ")"
		}
		state := c.Status
		if c.Conclusion != "" {
			state += " / " + c.Conclusion
		}
		details := c.Description
		if c.URL != "" {
			details += " " + c.URL
		}
		sha := c.HeadSHA
		if len(sha) > 8 {
			sha = sha[:8]
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", findingCell(c.Source), findingCell(c.Workflow), findingCell(c.Name), findingCell(state), findingCell(bypass), findingCell(sha), findingCell(limitPRText(details, 600)))
	}
	return b.String()
}

func (s *server) prMonitoringReply(ref string, enabled bool) botReply {
	if s.prs == nil || !s.prs.Enabled() {
		return botReply{text: "PR monitor is disabled."}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := s.prs.SetMonitoring(ctx, ref, enabled)
	if err != nil {
		return botReply{text: "Update PR monitoring failed: " + err.Error()}
	}
	return botReply{text: fmt.Sprintf("PR %s#%d monitoring=%t", d.PR.Repo, d.PR.Number, enabled)}
}

func (s *server) updatePRMonitoring(w http.ResponseWriter, r *http.Request) {
	if !s.requirePRs(w) {
		return
	}
	var body struct {
		Monitoring *bool `json:"monitoring"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Monitoring == nil {
		writeError(w, http.StatusBadRequest, errors.New("monitoring is required"))
		return
	}
	d, err := s.prs.SetMonitoring(r.Context(), r.PathValue("pr_id"), *body.Monitoring)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func limitPRText(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func (s *server) requirePRs(w http.ResponseWriter) bool {
	if s.prs == nil || !s.prs.Enabled() {
		writeError(w, http.StatusServiceUnavailable, errors.New("PR monitor is disabled"))
		return false
	}
	return true
}

func (s *server) listPRs(w http.ResponseWriter, r *http.Request) {
	if !s.requirePRs(w) {
		return
	}
	rows, err := s.prs.List(r.Context(), r.URL.Query().Get("all") == "1")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prs": rows})
}

func (s *server) getPR(w http.ResponseWriter, r *http.Request) {
	if !s.requirePRs(w) {
		return
	}
	d, err := s.prs.Get(r.Context(), r.PathValue("pr_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) trackPR(w http.ResponseWriter, r *http.Request) {
	if !s.requirePRs(w) {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	s.refreshPRRef(w, r, body.URL)
}

func (s *server) refreshPR(w http.ResponseWriter, r *http.Request) {
	if !s.requirePRs(w) {
		return
	}
	s.refreshPRRef(w, r, r.PathValue("pr_id"))
}

func (s *server) refreshPRRef(w http.ResponseWriter, r *http.Request, ref string) {
	d, err := s.prs.Refresh(r.Context(), ref)
	if d.PR.ID != "" {
		writeJSON(w, http.StatusOK, d)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeStoreError(w, err)
		return
	}
	if err == nil {
		err = errors.New("PR refresh failed")
	}
	writeError(w, http.StatusBadRequest, err)
}
