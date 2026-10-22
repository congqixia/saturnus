package lark

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
	_, err := c.SendTextGetID(receiveIDType, receiveID, text)
	return err
}

// SendTextGetID sends a plain text message and returns the created
// message_id, so the caller can rewrite it in place later (e.g. an ACK that
// is updated with the final result once a long-running command finishes).
func (c *Client) SendTextGetID(receiveIDType, receiveID, text string) (string, error) {
	if !c.Enabled() {
		return "", nil
	}
	payload := map[string]any{
		"receive_id": receiveID,
		"msg_type":   "text",
		"content":    mustMarshal(map[string]string{"text": text}),
	}
	url := "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=" + receiveIDType
	var out struct {
		Code int `json:"code"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := c.postJSONDecode(url, payload, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("lark send text failed: code=%d", out.Code)
	}
	return out.Data.MessageID, nil
}

// UpdateMessage rewrites the content of a previously sent text message
// (PUT /im/v1/messages/:message_id). Feishu only allows editing text/post
// messages sent by the app itself, up to 20 times per message.
func (c *Client) UpdateMessage(messageID, text string) error {
	if !c.Enabled() {
		return nil
	}
	if messageID == "" {
		return errors.New("empty message id")
	}
	payload := map[string]any{
		"msg_type": "text",
		"content":  mustMarshal(map[string]string{"text": text}),
	}
	url := "https://open.feishu.cn/open-apis/im/v1/messages/" + url.PathEscape(messageID)
	return c.sendJSON(http.MethodPut, url, payload)
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
	Summary       string
	Description   string
	Due           time.Time
	MemberOpenIDs []string
	SourceTitle   string
	SourceURL     string
}

func (c *Client) CreateTask(input CreateTaskInput) (Task, error) {
	if !c.Enabled() {
		return Task{}, nil
	}
	token, err := c.tenantToken()
	if err != nil {
		return Task{}, err
	}
	members := []map[string]any{}
	for _, id := range input.MemberOpenIDs {
		if id == "" {
			continue
		}
		members = append(members, map[string]any{"id": id, "type": "user", "role": "assignee"})
	}
	payload := map[string]any{
		"summary":     input.Summary,
		"description": input.Description,
		"members":     members,
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
		return Task{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Task{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Task struct {
				GUID string `json:"guid"`
				URL  string `json:"url"`
			} `json:"task"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Task{}, fmt.Errorf("lark create task failed: %s", larkErrorDetail(raw))
	}
	if resp.StatusCode >= 300 || out.Code != 0 {
		return Task{}, fmt.Errorf("lark create task failed: code=%d msg=%q status=%s", out.Code, out.Msg, resp.Status)
	}
	return Task{GUID: out.Data.Task.GUID, URL: out.Data.Task.URL}, nil
}

func (c *Client) postJSON(url string, payload any) error {
	return c.sendJSON(http.MethodPost, url, payload)
}

// postJSONDecode issues a POST request and decodes the response body into out.
func (c *Client) postJSONDecode(url string, payload any, out any) error {
	return c.sendJSONDecode(http.MethodPost, url, payload, out)
}

// sendJSON issues a JSON request with the tenant token. Empty payloads are
// allowed (e.g. a PATCH with an empty object).
func (c *Client) sendJSON(method, url string, payload any) error {
	return c.sendJSONDecode(method, url, payload, nil)
}

// sendJSONDecode issues a JSON request with the tenant token and, when out is
// non-nil, decodes the response body into it. Empty payloads are allowed (e.g.
// a PATCH with an empty object).
func (c *Client) sendJSONDecode(method, url string, payload any, out any) error {
	token, err := c.tenantToken()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
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
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("lark api failed: %s (%s)", resp.Status, larkErrorDetail(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
	}
	return nil
}

// larkErrorDetail extracts Feishu's error code and message from a response
// body, falling back to a truncated raw body snippet when it is not the usual
// {"code": ..., "msg": ...} JSON.
func larkErrorDetail(raw []byte) string {
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &out); err == nil && (out.Code != 0 || out.Msg != "") {
		return fmt.Sprintf("code=%d msg=%q", out.Code, out.Msg)
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return ""
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return "body=" + strconv.Quote(body)
}

// TaskList is a Feishu task group (任务分组) a task can be filed under.
type TaskList struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

// EnsureTaskList returns the task group with the given name, creating it when
// it does not exist (searched by name first to avoid duplicates). Requires the
// task:tasklist:read/write scopes.
func (c *Client) EnsureTaskList(name string) (TaskList, error) {
	if !c.Enabled() {
		return TaskList{}, errors.New("lark client disabled")
	}
	if name == "" {
		return TaskList{}, errors.New("empty tasklist name")
	}
	token, err := c.tenantToken()
	if err != nil {
		return TaskList{}, err
	}
	pageToken := ""
	for {
		u := "https://open.feishu.cn/open-apis/task/v2/tasklists?page_size=100"
		if pageToken != "" {
			u += "&page_token=" + url.QueryEscape(pageToken)
		}
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return TaskList{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return TaskList{}, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return TaskList{}, fmt.Errorf("lark list tasklists failed: %s (%s)", resp.Status, larkErrorDetail(raw))
		}
		var out struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items     []TaskList `json:"items"`
				PageToken string     `json:"page_token"`
				HasMore   bool       `json:"has_more"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return TaskList{}, err
		}
		if out.Code != 0 {
			return TaskList{}, fmt.Errorf("lark list tasklists failed: %s", out.Msg)
		}
		for _, item := range out.Data.Items {
			if item.Name == name {
				return item, nil
			}
		}
		if !out.Data.HasMore || out.Data.PageToken == "" {
			break
		}
		pageToken = out.Data.PageToken
	}
	payload := map[string]any{"name": name}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "https://open.feishu.cn/open-apis/task/v2/tasklists", bytes.NewReader(body))
	if err != nil {
		return TaskList{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return TaskList{}, err
	}
	defer resp.Body.Close()
	createdRaw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return TaskList{}, fmt.Errorf("lark create tasklist failed: %s (%s)", resp.Status, larkErrorDetail(createdRaw))
	}
	var created struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			TaskList TaskList `json:"tasklist"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createdRaw, &created); err != nil {
		return TaskList{}, err
	}
	if created.Code != 0 {
		return TaskList{}, fmt.Errorf("lark create tasklist failed: %s", created.Msg)
	}
	if created.Data.TaskList.GUID == "" {
		return TaskList{}, errors.New("lark create tasklist: no guid in response")
	}
	return created.Data.TaskList, nil
}

// AddTaskToList files an existing task under a task group (tasklist).
func (c *Client) AddTaskToList(taskGUID, taskListGUID string) error {
	if !c.Enabled() {
		return errors.New("lark client disabled")
	}
	if taskGUID == "" || taskListGUID == "" {
		return errors.New("empty task/tasklist guid")
	}
	token, err := c.tenantToken()
	if err != nil {
		return err
	}
	payload := map[string]any{"tasklist_guid": taskListGUID}
	body, _ := json.Marshal(payload)
	u := "https://open.feishu.cn/open-apis/task/v2/tasks/" + url.PathEscape(taskGUID) + "/add_tasklist?user_id_type=open_id"
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
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
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("lark add task to tasklist failed: %s (%s)", resp.Status, larkErrorDetail(raw))
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("lark add task to tasklist failed: %s", out.Msg)
	}
	return nil
}

// Task is the subset of a Feishu task v2 object the server cares about.
type Task struct {
	GUID        string
	URL         string
	Summary     string
	Description string
	Completed   bool
}

// GetTask fetches a Feishu task by guid.
func (c *Client) GetTask(taskGUID string) (Task, error) {
	if !c.Enabled() {
		return Task{}, errors.New("lark client disabled")
	}
	if taskGUID == "" {
		return Task{}, errors.New("empty task guid")
	}
	token, err := c.tenantToken()
	if err != nil {
		return Task{}, err
	}
	req, err := http.NewRequest(http.MethodGet, "https://open.feishu.cn/open-apis/task/v2/tasks/"+url.PathEscape(taskGUID), nil)
	if err != nil {
		return Task{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Task{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return Task{}, fmt.Errorf("lark get task failed: %s (%s)", resp.Status, larkErrorDetail(raw))
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Task struct {
				GUID        string `json:"guid"`
				URL         string `json:"url"`
				Summary     string `json:"summary"`
				Description string `json:"description"`
				Completed   bool   `json:"completed"`
			} `json:"task"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Task{}, err
	}
	if out.Code != 0 {
		return Task{}, fmt.Errorf("lark get task failed: %s", out.Msg)
	}
	return Task{
		GUID:        out.Data.Task.GUID,
		URL:         out.Data.Task.URL,
		Summary:     out.Data.Task.Summary,
		Description: out.Data.Task.Description,
		Completed:   out.Data.Task.Completed,
	}, nil
}

// UpdateTaskInput carries the optional fields of a task v2 PATCH update.
// nil fields are left untouched on the remote task.
type UpdateTaskInput struct {
	Summary     *string
	Description *string
	Completed   *bool
}

// taskDescriptionMaxRunes is the Feishu task v2 description limit (3000 UTF-8
// characters). Longer text is truncated before sending.
const taskDescriptionMaxRunes = 3000

// UpdateTask patches an existing Feishu task (summary / description /
// completion). The task v2 update contract requires a `task` object plus an
// `update_fields` list naming exactly the fields being changed; completion is
// expressed via `completed_at` (ms timestamp, or "0" to reopen). Requires the
// task:task write scope.
func (c *Client) UpdateTask(taskGUID string, input UpdateTaskInput) error {
	if !c.Enabled() {
		return nil
	}
	if taskGUID == "" {
		return errors.New("empty task guid")
	}
	payload, ok := buildTaskUpdate(input)
	if !ok {
		return nil
	}
	return c.sendJSON(http.MethodPatch, "https://open.feishu.cn/open-apis/task/v2/tasks/"+url.PathEscape(taskGUID), payload)
}

// buildTaskUpdate renders the task v2 PATCH request body: the fields to change
// go in the nested `task` object and their names in `update_fields`. The bool
// result reports whether anything is to be updated.
func buildTaskUpdate(input UpdateTaskInput) (map[string]any, bool) {
	task := map[string]any{}
	updateFields := []string{}
	if input.Summary != nil {
		task["summary"] = *input.Summary
		updateFields = append(updateFields, "summary")
	}
	if input.Description != nil {
		task["description"] = truncateRunes(*input.Description, taskDescriptionMaxRunes)
		updateFields = append(updateFields, "description")
	}
	if input.Completed != nil {
		if *input.Completed {
			task["completed_at"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
		} else {
			task["completed_at"] = "0"
		}
		updateFields = append(updateFields, "completed_at")
	}
	if len(updateFields) == 0 {
		return nil, false
	}
	return map[string]any{
		"task":          task,
		"update_fields": updateFields,
	}, true
}

// CompleteTask marks a Feishu task as completed.
func (c *Client) CompleteTask(taskGUID string) error {
	completed := true
	return c.UpdateTask(taskGUID, UpdateTaskInput{Completed: &completed})
}

// truncateRunes bounds a string to max UTF-8 characters without splitting a
// rune.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// BotInfo is the identity of the current app's bot, resolved from
// bot/v3/info.
type BotInfo struct {
	OpenID string
	Name   string
}

// BotInfo resolves the app bot's own open_id via bot/v3/info. The bot
// object is tolerated at both "bot" (classic shape) and "data.bot", since
// the endpoint's nesting has varied. It is used to detect whether a group
// message directly mentions the bot.
func (c *Client) BotInfo() (BotInfo, error) {
	if !c.Enabled() {
		return BotInfo{}, errors.New("lark client disabled")
	}
	token, err := c.tenantToken()
	if err != nil {
		return BotInfo{}, err
	}
	req, err := http.NewRequest(http.MethodGet, "https://open.feishu.cn/open-apis/bot/v3/info", nil)
	if err != nil {
		return BotInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return BotInfo{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return BotInfo{}, fmt.Errorf("lark bot info failed: %s (%s)", resp.Status, larkErrorDetail(raw))
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			AppName string `json:"app_name"`
		} `json:"bot"`
		Data struct {
			Bot struct {
				OpenID  string `json:"open_id"`
				AppName string `json:"app_name"`
			} `json:"bot"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return BotInfo{}, err
	}
	if out.Code != 0 {
		return BotInfo{}, fmt.Errorf("lark bot info failed: %s", out.Msg)
	}
	bot := out.Bot
	if bot.OpenID == "" {
		bot = out.Data.Bot
	}
	if bot.OpenID == "" {
		return BotInfo{}, errors.New("lark bot info: no open_id in response")
	}
	return BotInfo{OpenID: bot.OpenID, Name: bot.AppName}, nil
}

// Chat is the subset of a Feishu chat the server cares about when listing the
// groups the bot has joined. Group chats are identified by chat_mode
// ("group"/"topic"); chat_type is only present on the single-chat response and
// uses different values (e.g. "private"), so it is not a reliable discriminator.
type Chat struct {
	ChatID   string `json:"chat_id"`
	Name     string `json:"name"`
	ChatMode string `json:"chat_mode"`
}

// IsGroupChat reports whether the chat is a multi-member group (a normal
// group or a topic group).
func (ch Chat) IsGroupChat() bool {
	switch ch.ChatMode {
	case "group", "topic":
		return true
	}
	return false
}

// IsKnownNonGroup reports whether the chat is definitely not a group. An empty
// chat_mode means the type is unknown, which is treated as not-non-group so
// validation stays lenient.
func (ch Chat) IsKnownNonGroup() bool {
	return ch.ChatMode != "" && !ch.IsGroupChat()
}

// ListChats returns the chats the bot is a member of, following pagination.
func (c *Client) ListChats() ([]Chat, error) {
	if !c.Enabled() {
		return nil, errors.New("lark client disabled")
	}
	token, err := c.tenantToken()
	if err != nil {
		return nil, err
	}
	var chats []Chat
	pageToken := ""
	for {
		u := "https://open.feishu.cn/open-apis/im/v1/chats?page_size=100"
		if pageToken != "" {
			u += "&page_token=" + url.QueryEscape(pageToken)
		}
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("lark list chats failed: %s (%s)", resp.Status, larkErrorDetail(body))
		}
		var out struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items     []Chat `json:"items"`
				PageToken string `json:"page_token"`
				HasMore   bool   `json:"has_more"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		if out.Code != 0 {
			return nil, fmt.Errorf("lark list chats failed: %s", out.Msg)
		}
		chats = append(chats, out.Data.Items...)
		if !out.Data.HasMore || out.Data.PageToken == "" {
			break
		}
		pageToken = out.Data.PageToken
	}
	return chats, nil
}

// GetChat fetches one chat the bot can see. It is used to validate an
// explicit chat_id given outside the chat (e.g. when setting a group mode).
func (c *Client) GetChat(chatID string) (Chat, error) {
	if !c.Enabled() {
		return Chat{}, errors.New("lark client disabled")
	}
	token, err := c.tenantToken()
	if err != nil {
		return Chat{}, err
	}
	req, err := http.NewRequest(http.MethodGet, "https://open.feishu.cn/open-apis/im/v1/chats/"+url.PathEscape(chatID), nil)
	if err != nil {
		return Chat{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Chat{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return Chat{}, fmt.Errorf("lark get chat failed: %s (%s)", resp.Status, larkErrorDetail(raw))
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Chat Chat `json:"chat"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Chat{}, err
	}
	if out.Code != 0 {
		return Chat{}, fmt.Errorf("lark get chat failed: %s", out.Msg)
	}
	return out.Data.Chat, nil
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
	name, err := parseContactUserName(raw)
	if err != nil {
		return "", err
	}
	if name == "" {
		// Debug aid: the API answered with code 0 but no name was extracted.
		// A user object containing only identity fields (open_id/union_id/user_id)
		// means the app lacks the field-level permission to read names, i.e. the
		// contact:user.base:readonly scope (获取用户基本信息) is not granted.
		fmt.Fprintf(os.Stderr, "lark: contact user %s returned no name fields (likely missing contact:user.base:readonly scope); response: %.800s\n", openID, string(raw))
	}
	return name, nil
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
