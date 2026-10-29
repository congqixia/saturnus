package prs

import (
	"os"
	"path/filepath"
	"testing"

	"saturnus/internal/store"
)

func TestBypassDoesNotRewriteUpstreamOrHideOtherFailures(t *testing.T) {
	policy := Policy{Global: []string{"optional"}, Repos: map[string][]string{"a/b": {"repo optional"}}}
	checks := []store.PRCheck{
		{Name: "optional", Status: "completed", Conclusion: "failure", Bucket: "fail"},
		{Name: "repo optional", Status: "pending", Bucket: "pending"},
		{Name: "required", Status: "completed", Conclusion: "failure", Bucket: "fail"},
	}
	state, counts := policy.Evaluate("A/B", checks)
	if state != "FAIL" || counts.Fail != 1 || counts.Bypass != 2 || counts.Pending != 0 {
		t.Fatalf("incorrect effective result: %s %+v", state, counts)
	}
	if checks[0].Bucket != "fail" || checks[0].Conclusion != "failure" || checks[0].BypassRule != "global" || checks[1].BypassRule != "repo:a/b" || checks[2].Bypassed {
		t.Fatalf("upstream result changed: %+v", checks)
	}
	checks[2].Conclusion, checks[2].Bucket = "success", "pass"
	if state, _ := policy.Evaluate("a/b", checks); state != "PASS" {
		t.Fatal(state)
	}
	if state, _ := policy.Evaluate("other/repo", checks); state != "PENDING" {
		t.Fatalf("repo rule leaked: %s", state)
	}
	if policy.Rule("a/b", "Optional") != "" || policy.Rule("a/b", "optional-other") != "" {
		t.Fatal("bypass must match names exactly")
	}
	if state, _ := policy.Evaluate("a/b", checks[:2]); state != "BYPASS" {
		t.Fatalf("all bypass must not be PASS: %s", state)
	}
	if state, _ := policy.Evaluate("a/b", nil); state != "NO_CHECKS" {
		t.Fatal(state)
	}
	for _, tc := range []struct{ bucket, want string }{{"skip", "SKIP"}, {"cancel", "CANCEL"}, {"pending", "PENDING"}, {"unknown", "UNKNOWN"}} {
		if state, _ := policy.Evaluate("a/b", []store.PRCheck{{Name: "required", Bucket: tc.bucket}}); state != tc.want {
			t.Fatalf("%s -> %s", tc.bucket, state)
		}
	}
}

func TestBypassConfigStrictNamesAndRepositoryScope(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"global":["name with space"],"repos":{"A/B":["ci-v2/test"]}}`, true},
		{`{}`, true}, {`null`, false}, {`[]`, false}, {`{"global":[""]}`, false},
		{`{"global":[" padded "]}`, false}, {`{"global":["a","a"]}`, false},
		{`{"typo":["a"]}`, false}, {`{"repos":{"../repo":["x"]}}`, false},
		{`{"repos":{"A/B":[],"a/b":[]}}`, false}, {`{} {}`, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "bypass.json")
			if err := os.WriteFile(file, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := LoadPolicy(file)
			if (err == nil) != tc.valid {
				t.Fatalf("policy=%+v err=%v", p, err)
			}
			if tc.valid && len(p.Global) > 0 && (p.Rule("other/repo", "name with space") != "global" || p.Rule("a/b", "ci-v2/test") != "repo:a/b") {
				t.Fatal(p)
			}
		})
	}
	if _, err := LoadPolicy(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing config silently ignored")
	}
}
