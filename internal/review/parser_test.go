package review

import "testing"

func TestFindPRRequest(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantOK   bool
		wantRepo string
		wantNum  int
	}{
		{
			name:     "full url",
			text:     "please review https://github.com/congqixia/saturnus/pull/42",
			wantOK:   true,
			wantRepo: "congqixia/saturnus",
			wantNum:  42,
		},
		{
			name:     "bare url",
			text:     "/sat review github.com/foo/bar/pull/7",
			wantOK:   true,
			wantRepo: "foo/bar",
			wantNum:  7,
		},
		{
			name:     "with path suffix",
			text:     "review https://github.com/a/b/pull/123/files",
			wantOK:   true,
			wantRepo: "a/b",
			wantNum:  123,
		},
		{
			name:   "no pr url",
			text:   "what is the weather",
			wantOK: false,
		},
		{
			name:   "not github",
			text:   "review https://gitlab.com/a/b/merge_requests/1",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := FindPRRequest(tc.text)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if req.Repo != tc.wantRepo || req.PRNumber != tc.wantNum {
					t.Fatalf("got %s#%d, want %s#%d", req.Repo, req.PRNumber, tc.wantRepo, tc.wantNum)
				}
				if req.PRURL == "" {
					t.Fatal("expected canonical pr_url")
				}
			} else if err == nil {
				t.Fatalf("expected error, got %#v", req)
			}
		})
	}
}
