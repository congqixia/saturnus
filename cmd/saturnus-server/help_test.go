package main

import (
	"strings"
	"testing"

	"saturnus/internal/review"
)

func TestHelpDirectory(t *testing.T) {
	srv := &server{}
	for _, command := range []string{"/sat", "/sat help"} {
		reply := srv.handleBotCommand(command, review.MessageContext{})
		if reply.md == nil || reply.md.Title != "Saturnus Help" {
			t.Fatalf("%s: missing directory card: %+v", command, reply)
		}
		for _, text := range []string{reply.text, reply.md.Content} {
			for _, module := range []string{"review", "pr", "task", "session", "admin"} {
				if !strings.Contains(text, "/sat help "+module) {
					t.Errorf("%s: directory missing module %s", command, module)
				}
			}
			if strings.Contains(text, "/sat review <") || strings.Contains(text, "/sat task add-") || len(text) > 1000 {
				t.Errorf("%s: directory should only contain short module summaries: %s", command, text)
			}
		}
	}
}

func TestHelpModules(t *testing.T) {
	srv := &server{}
	for _, tc := range []struct {
		module  string
		command string
	}{
		{"review", "/sat review <pr-url|review_id> [@user]"},
		{"pr", "/sat prs [refresh|all]"},
		{"task", "/sat task create <issue-url>"},
		{"session", "/sat approve <request_id>"},
		{"admin", "/sat group [chat_id]"},
	} {
		t.Run(tc.module, func(t *testing.T) {
			reply := srv.handleBotCommand("/sat help "+tc.module, review.MessageContext{})
			if reply.md == nil || !strings.Contains(reply.text, tc.command) || !strings.Contains(reply.md.Content, "**"+tc.command+"**") {
				t.Fatalf("missing module commands: %+v", reply)
			}
			for _, module := range satHelpModules {
				for _, command := range module.commands {
					line := command.command + " - " + command.description + "\n"
					mdLine := "**" + command.command + "** — " + command.description + "\n"
					want := 0
					if module.name == tc.module {
						want = 1
					}
					if strings.Count(reply.text, line) != want || strings.Count(reply.md.Content, mdLine) != want {
						t.Errorf("%s: command %q should appear %d times", tc.module, command.command, want)
					}
				}
			}
			if !strings.Contains(reply.text, "/sat help to view all modules") || !strings.Contains(reply.md.Content, "/sat help to view all modules") {
				t.Error("missing link back to module directory")
			}
		})
	}
}

func TestHelpInvalidModule(t *testing.T) {
	srv := &server{}
	for _, tc := range []struct {
		command string
		want    string
	}{
		{"/sat help missing", `Unknown help module "missing".`},
		{"/sat help review extra", "Usage: /sat help [module]"},
	} {
		reply := srv.handleBotCommand(tc.command, review.MessageContext{})
		if reply.md == nil {
			t.Fatalf("%s: missing help card", tc.command)
		}
		for _, output := range []struct{ text, want string }{
			{reply.text, tc.want},
			{reply.md.Content, escapeMarkdown(tc.want)},
		} {
			if !strings.Contains(output.text, output.want) || !strings.Contains(output.text, "/sat help review") || !strings.Contains(output.text, "/sat help admin") {
				t.Errorf("%s: expected error and module directory: %s", tc.command, output.text)
			}
		}
	}
}
