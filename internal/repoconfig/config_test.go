package repoconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadInlineCompatibility(t *testing.T) {
	repos, err := Load(" a/b=./relative , c/d = /srv/c=x ,bad,=missing,empty=,a/b=../last,,", "")
	want := map[string]string{"a/b": "../last", "c/d": "/srv/c=x"}
	if err != nil || !reflect.DeepEqual(repos, want) {
		t.Fatalf("repos=%v err=%v, want %v", repos, err, want)
	}
	repos, err = Load("", "")
	if err != nil || len(repos) != 0 {
		t.Fatalf("empty config: repos=%v err=%v", repos, err)
	}
}

func TestLoadFilePaths(t *testing.T) {
	base := t.TempDir()
	abs := filepath.Join(base, "absolute checkout,with=punctuation")
	want := map[string]string{
		"owner/absolute": abs,
		"owner/relative": filepath.Join(base, "..", "checkout,with=punctuation"),
		"owner/literal":  filepath.Join(base, "~", "$CHECKOUT"),
		"owner/spaces":   filepath.Join(base, " checkout "),
	}
	data, err := json.Marshal(map[string]string{
		"owner/absolute": abs,
		"owner/relative": "../checkout,with=punctuation",
		"owner/literal":  "~/$CHECKOUT",
		"owner/spaces":   " checkout ",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, base, string(data))
	// A relative config filename must still resolve checkout paths against the
	// config's directory, independently of the process working directory.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Rel(cwd, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHECKOUT", "should-not-be-expanded")
	repos, err := Load("", path)
	if err != nil || !reflect.DeepEqual(repos, want) {
		t.Fatalf("repos=%v err=%v, want %v", repos, err, want)
	}
	// No checkout directories were created: file loading only validates config.
}

func TestLoadFileValidation(t *testing.T) {
	for _, tc := range []struct {
		name, data, message string
	}{
		{"empty", "", "EOF"},
		{"array", "[]", "expected a JSON object"},
		{"null", "null", "expected a JSON object"},
		{"string", `"a/b=/checkout"`, "expected a JSON object"},
		{"malformed", `{"a/b":`, "EOF"},
		{"unclosed", `{"a/b":"checkout"`, "EOF"},
		{"trailing comma", `{"a/b":"checkout",}`, "invalid character"},
		{"trailing object", `{} {}`, "unexpected content"},
		{"trailing garbage", `{} garbage`, "invalid character"},
		{"duplicate", `{"a/b":"first","a/b":"second"}`, `duplicate repo "a/b"`},
		{"escaped duplicate", `{"a/b":"first","a\u002fb":"second"}`, `duplicate repo "a/b"`},
		{"empty repo", `{"":"checkout"}`, "invalid repo"},
		{"owner only", `{"owner":"checkout"}`, "invalid repo"},
		{"empty name", `{"owner/":"checkout"}`, "invalid repo"},
		{"empty owner", `{"/repo":"checkout"}`, "invalid repo"},
		{"url", `{"https://github.com/a/b":"checkout"}`, "invalid repo"},
		{"extra slash", `{"a/b/c":"checkout"}`, "invalid repo"},
		{"whitespace repo", `{"a/ b":"checkout"}`, "invalid repo"},
		{"dot component", `{"a/..":"checkout"}`, "invalid repo"},
		{"number path", `{"a/b":123}`, "checkout path string"},
		{"object path", `{"a/b":{"dir":"checkout"}}`, "checkout path string"},
		{"array path", `{"a/b":[]}`, "checkout path string"},
		{"null path", `{"a/b":null}`, "non-empty string"},
		{"empty path", `{"a/b":""}`, "non-empty string"},
		{"blank path", `{"a/b":" \t"}`, "non-empty string"},
		{"partial invalid", `{"a/b":"checkout","c/d":false}`, `repo "c/d"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, t.TempDir(), tc.data)
			repos, err := Load("", path)
			if err == nil || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), path) {
				t.Fatalf("repos=%v err=%v, want filename and %q", repos, err, tc.message)
			}
			if repos != nil {
				t.Fatalf("invalid file returned partial mapping: %v", repos)
			}
		})
	}
}

func TestLoadSources(t *testing.T) {
	base := t.TempDir()
	path := writeConfig(t, base, "{}\n\t ")
	if repos, err := Load(" \t", path); err != nil || len(repos) != 0 {
		t.Fatalf("empty JSON object: repos=%v err=%v", repos, err)
	}
	for _, file := range []string{path, filepath.Join(base, "missing.json")} {
		if repos, err := Load("a/b=/checkout", file); repos != nil || err == nil || !strings.Contains(err.Error(), "sources conflict") {
			t.Fatalf("conflicting config: repos=%v err=%v", repos, err)
		}
	}
	for _, file := range []string{base, filepath.Join(base, "missing.json")} {
		if repos, err := Load("", file); repos != nil || err == nil || !strings.Contains(err.Error(), "read repo config") {
			t.Fatalf("unreadable config: repos=%v err=%v", repos, err)
		}
	}
}

func writeConfig(t *testing.T, base, data string) string {
	t.Helper()
	path := filepath.Join(base, "repos.json")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
