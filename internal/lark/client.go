package lark

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	AppID     string
	AppSecret string
	Enabled   bool
}

type Client struct {
	cfg        Config
	httpClient *http.Client
	token      string
	tokenExp   time.Time
}

func New(cfg Config) *Client {
	cfg.Enabled = cfg.AppID != "" && cfg.AppSecret != ""
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Enabled() bool {
	return c != nil && c.cfg.Enabled
}

func (c *Client) SendText(receiveIDType, receiveID, text string) error {
	if !c.Enabled() {
		return nil
	}
	payload := map[string]any{
		"receive_id": receiveID,
		"msg_type":   "text",
		"content":    mustMarshal(map[string]string{"text": text}),
	}
	url := "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=" + receiveIDType
	return c.postJSON(url, payload)
}

func (c *Client) SendTextToUser(openID, text string) error {
	return c.SendText("open_id", openID, text)
}

func (c *Client) ReplyMessage(messageID, text string) error {
	if !c.Enabled() {
		return nil
	}
	payload := map[string]any{
		"msg_type": "text",
		"content":  mustMarshal(map[string]string{"text": text}),
	}
	url := "https://open.feishu.cn/open-apis/im/v1/messages/" + messageID + "/reply"
	return c.postJSON(url, payload)
}

type CreateTaskInput struct {
	Summary      string
	Description  string
	Due          time.Time
	MemberOpenID string
	SourceTitle  string
	SourceURL    string
}

func (c *Client) CreateTask(input CreateTaskInput) (string, error) {
	if !c.Enabled() {
		return "", nil
	}
	token, err := c.tenantToken()
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"summary":     input.Summary,
		"description": input.Description,
		"members":     []map[string]any{{"id": input.MemberOpenID, "type": "user"}},
	}
	if !input.Due.IsZero() {
		payload["due"] = map[string]any{"timestamp": fmt.Sprintf("%d", input.Due.UnixMilli())}
	}
	if input.SourceTitle != "" {
		payload["source"] = map[string]any{"title": input.SourceTitle, "url": input.SourceURL}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "https://open.feishu.cn/open-apis/task/v2/tasks", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Task struct {
				GUID string `json:"guid"`
			} `json:"task"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 || out.Code != 0 {
		return "", fmt.Errorf("lark create task failed: %s %s", resp.Status, out.Msg)
	}
	return out.Data.Task.GUID, nil
}

func (c *Client) postJSON(url string, payload any) error {
	token, err := c.tenantToken()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("lark api failed: %s", resp.Status)
	}
	return nil
}

type ContactUser struct {
	OpenID string `json:"open_id"`
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Mobile string `json:"mobile"`
}

func (c *Client) GetUserName(openID string) (string, error) {
	if !c.Enabled() {
		return "", errors.New("lark client disabled")
	}
	token, err := c.tenantToken()
	if err != nil {
		return "", err
	}
	url := "https://open.feishu.cn/open-apis/contact/v3/users/" + url.PathEscape(openID) + "?user_id_type=open_id"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("lark contact get failed: %s", resp.Status)
	}
	return parseContactUserName(raw)
}

func parseContactUserName(raw []byte) (string, error) {
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			User struct {
				Name     string `json:"name"`
				ENName   string `json:"en_name"`
				Nickname string `json:"nickname"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("lark contact get failed: %s", out.Msg)
	}
	switch {
	case out.Data.User.Name != "":
		return out.Data.User.Name, nil
	case out.Data.User.ENName != "":
		return out.Data.User.ENName, nil
	case out.Data.User.Nickname != "":
		return out.Data.User.Nickname, nil
	}
	return "", nil
}

func (c *Client) ResolveUser(query string) ([]ContactUser, error) {
	if !c.Enabled() {
		return nil, errors.New("lark client disabled")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty query")
	}
	token, err := c.tenantToken()
	if err != nil {
		return nil, err
	}
	switch {
	case strings.Contains(query, "@"):
		return c.batchGetIDs(token, []string{query}, nil)
	case isAllDigits(query):
		return c.batchGetIDs(token, nil, []string{query})
	default:
		return c.searchUsers(token, query)
	}
}

func (c *Client) batchGetIDs(token string, emails, mobiles []string) ([]ContactUser, error) {
	payload := map[string]any{}
	if len(emails) > 0 {
		payload["emails"] = emails
	}
	if len(mobiles) > 0 {
		payload["mobiles"] = mobiles
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "https://open.feishu.cn/open-apis/contact/v3/users/batch_get_id", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lark batch_get_id failed: %s", resp.Status)
	}
	return parseContactUsers(raw)
}

func (c *Client) searchUsers(token, query string) ([]ContactUser, error) {
	url := "https://open.feishu.cn/open-apis/contact/v3/users/search?query=" + url.QueryEscape(query) + "&page_size=10&user_id_type=open_id"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lark users/search failed: %s", resp.Status)
	}
	return parseContactUsers(raw)
}

func parseContactUsers(raw []byte) ([]ContactUser, error) {
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			UserList []json.RawMessage `json:"user_list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("lark contact resolve failed: %s", out.Msg)
	}
	users := make([]ContactUser, 0, len(out.Data.UserList))
	for _, item := range out.Data.UserList {
		var u ContactUser
		if err := json.Unmarshal(item, &u); err != nil {
			return nil, err
		}
		if u.OpenID == "" {
			u.OpenID = u.UserID
		}
		users = append(users, u)
	}
	return users, nil
}

func isAllDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(value) > 0
}

func (c *Client) tenantToken() (string, error) {
	if c.token != "" && time.Now().Before(c.tokenExp.Add(-time.Minute)) {
		return c.token, nil
	}
	payload := map[string]string{"app_id": c.cfg.AppID, "app_secret": c.cfg.AppSecret}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("lark tenant token failed: %s", out.Msg)
	}
	c.token = out.TenantAccessToken
	c.tokenExp = time.Now().Add(time.Duration(out.Expire) * time.Second)
	return c.token, nil
}

func mustMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
