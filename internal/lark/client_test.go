package lark

import "testing"

func TestParseContactUserName(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "name", raw: `{"code":0,"data":{"user":{"name":"张三"}}}`, want: "张三"},
		{name: "en name fallback", raw: `{"code":0,"data":{"user":{"en_name":"Zhang San"}}}`, want: "Zhang San"},
		{name: "nickname fallback", raw: `{"code":0,"data":{"user":{"nickname":"zs"}}}`, want: "zs"},
		{name: "empty", raw: `{"code":0,"data":{"user":{}}}`, want: ""},
		{name: "error code", raw: `{"code":99991672,"msg":"no permission"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseContactUserName([]byte(tc.raw))
			if tc.want == "" && tc.name == "error code" {
				if err == nil {
					t.Fatal("expected error for non-zero code")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseContactUsers(t *testing.T) {
	raw := []byte(`{
		"code": 0,
		"msg": "ok",
		"data": {
			"user_list": [
				{"user_id": "ou_email_user", "email": "a@example.com", "mobile": "13800000001"},
				{"open_id": "ou_name_user", "name": "Zhang San", "email": "zs@example.com"},
				{"user_id": "ou_no_contact"}
			]
		}
	}`)
	users, err := parseContactUsers(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	if users[0].OpenID != "ou_email_user" {
		t.Fatalf("expected user_id fallback as open_id, got %q", users[0].OpenID)
	}
	if users[1].OpenID != "ou_name_user" || users[1].Name != "Zhang San" {
		t.Fatalf("unexpected search result: %#v", users[1])
	}
}

func TestParseContactUsersError(t *testing.T) {
	raw := []byte(`{"code": 99991672, "msg": "no permission"}`)
	if _, err := parseContactUsers(raw); err == nil {
		t.Fatal("expected error for non-zero code")
	}
}

func TestIsAllDigits(t *testing.T) {
	if !isAllDigits("13800000001") {
		t.Fatal("expected true for numeric mobile")
	}
	if isAllDigits("138-0000") {
		t.Fatal("expected false for mixed value")
	}
	if isAllDigits("") {
		t.Fatal("expected false for empty")
	}
}
