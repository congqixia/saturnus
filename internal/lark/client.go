package lark

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
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
	token, err := c.tenantToken()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"receive_id": receiveID,
		"msg_type":   "text",
		"content":    mustMarshal(map[string]string{"text": text}),
	}
	body, _ := json.Marshal(payload)
	url := "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=" + receiveIDType
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
		return fmt.Errorf("lark send message failed: %s", resp.Status)
	}
	return nil
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
