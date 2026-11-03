package main

import (
	"fmt"
	"strings"

	"saturnus/internal/lark"
)

type satHelpCommand struct {
	command     string
	description string
}

type satHelpModule struct {
	name        string
	title       string
	description string
	commands    []satHelpCommand
}

// satHelpModules defines the help directory and each module's commands.
// Keep it in sync with handleBotCommand and the README.
var satHelpModules = []satHelpModule{
	{name: "review", title: "PR Reviews", description: "start reviews, inspect findings, and follow up", commands: []satHelpCommand{
		{"/sat reviews", "list recent PR reviews (archived reviews are hidden)"},
		{"/sat reviews all|archived", "list PR reviews including archived, or only archived"},
		{"/sat review <pr-url|review_id> [@user]", "start a review (optionally attribute the requester to @user), or show one review by id"},
		{"/sat retry <review_id>", "retry a failed review, or re-run after the PR head changed"},
		{"/sat reset <review_id>", "reset the tool session and queue a fresh review context, even when the PR head is unchanged"},
		{"/sat post <review_id>", "publish the review's findings as GitHub inline comments (plus the verdict review); for reviews whose findings were never posted"},
		{"/sat refresh <review_id|all>", "sync PR/task state: archive merged/closed PRs, complete their task, append reviewer comments to the task"},
		{"/sat act <review_id> <instruction>", "resume the review session to act on the PR (e.g. inline comments for selected issues, concrete code for an issue)"},
		{"/sat threads <review_id>", "list all chat threads a review was requested in"},
		{"/sat describe review <review_id>", "show review details with finding counts"},
		{"/sat findings <review_id> [page]", "show findings with status and review round (5 per page)"},
		{"/sat notifications <review_id> [page]", "show requester result notifications and delivery status (one per page)"},
		{"/sat repos", "show the configured review repos"},
		{"/sat reply <review_id> <message>", "reviewer replies to the requester"},
		{"/sat requester <review_id> @user|ou_xxx", "update a review's requester (mention the user, or pass an open_id)"},
	}},
	{name: "pr", title: "PR & CI", description: "track pull requests and inspect CI checks", commands: []satHelpCommand{
		{"/sat prs [refresh|all]", "list tracked PRs with CI and bypass counts; refresh discovers and polls all whitelisted PRs"},
		{"/sat pr <pr-url|owner/repo#number|pr_id>", "track/refresh a PR and show CI check details, including bypass policy"},
		{"/sat pr pause|resume <pr-url|owner/repo#number|pr_id>", "pause or resume local CI polling for one PR"},
		{"/sat describe pr <pr-url|owner/repo#number|pr_id>", "show a PR and its CI checks"},
	}},
	{name: "task", title: "Feature Tasks", description: "manage tasks and their related items", commands: []satHelpCommand{
		{"/sat tasks", "list feature tasks (archived tasks are hidden)"},
		{"/sat tasks all|archived", "list tasks including archived, or only archived"},
		{"/sat task <task_id> [page]", "show task details and related items"},
		{"/sat task add-ref <task_id> <url> [title]", "attach a reference URL"},
		{"/sat task rm-ref <task_id> <ref_id>", "detach a reference URL"},
		{"/sat task create <issue-url>", "create a task from a GitHub issue"},
		{"/sat task create-title <title>", "create a task without an issue"},
		{"/sat task bind-issue <task_id> <issue-url>", "bind an issue to a task"},
		{"/sat task add-session <task_id> <tool> <session_id> [purpose]", "link a tool session"},
		{"/sat task rm-session <task_id> <link_id>", "unlink a tool session"},
		{"/sat task add-doc <task_id> <owner/repo:path>", "attach a local design doc to a task"},
		{"/sat task rm-doc <task_id> <doc_id>", "detach a design doc"},
		{"/sat task add-pr <task_id> <pr-url>", "attach a PR and snapshot its review/Feishu-task state"},
		{"/sat task rm-pr <task_id> <pr_id>", "detach a PR"},
		{"/sat task refresh <task_id|all>", "re-sync issue/PR/review/Feishu-task state"},
		{"/sat task close <task_id>", "archive a task"},
	}},
	{name: "session", title: "Sessions & Approvals", description: "inspect tool sessions and manage approvals", commands: []satHelpCommand{
		{"/sat sessions", "list recent sessions"},
		{"/sat pending", "list pending approvals"},
		{"/sat session <session_id>", "show one session"},
		{"/sat approve <request_id>", "approve a pending request"},
		{"/sat deny <request_id> <reason>", "deny a pending request"},
		{"/sat autopass on|off <session_id>", "toggle auto-approve for a session"},
	}},
	{name: "admin", title: "Groups & Users", description: "configure groups and look up users", commands: []satHelpCommand{
		{"/sat groups", "list groups the bot has joined and their modes"},
		{"/sat group [chat_id]", "show a group's mode (defaults to the current group)"},
		{"/sat group mode <flags> [chat_id]", "set a group's mode flags (review, report, or report|review); omit chat_id when running inside the group"},
		{"/sat whoami", "show your open_id"},
		{"/sat whois <name|email|mobile>", "resolve a user to open_id"},
	}},
}

func helpReply(args []string) botReply {
	title := "Saturnus Help"
	var commands []satHelpCommand
	for _, module := range satHelpModules {
		commands = append(commands, satHelpCommand{"/sat help " + module.name, module.description})
	}
	prefix := ""
	footer := "Use /sat help <module> to view commands."
	if len(args) > 1 {
		prefix = "Usage: /sat help [module]\n\n"
	} else if len(args) == 1 {
		found := false
		for _, module := range satHelpModules {
			if module.name == args[0] {
				title += " — " + module.title
				commands = module.commands
				footer = "Use /sat help to view all modules."
				found = true
				break
			}
		}
		if !found {
			prefix = fmt.Sprintf("Unknown help module %q.\n\n", args[0])
		}
	}
	var text, md strings.Builder
	text.WriteString(prefix)
	md.WriteString(escapeMarkdown(prefix))
	for _, command := range commands {
		fmt.Fprintf(&text, "%s - %s\n", command.command, command.description)
		fmt.Fprintf(&md, "**%s** — %s\n", command.command, command.description)
	}
	fmt.Fprintf(&text, "\n%s", footer)
	fmt.Fprintf(&md, "\n%s", footer)
	return botReply{
		text: text.String(),
		md:   &lark.MarkdownCard{Title: title, Content: md.String()},
	}
}
