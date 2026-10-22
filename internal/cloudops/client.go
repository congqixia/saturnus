package cloudops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Client queries the cloud-ops console API using a file-based Config.
type Client struct {
	cfg        *Config
	httpClient *http.Client
}

// Action is one record from the instance action history. Only fields needed to
// build the reply are parsed; the display mapping (columns, labels, links)
// comes from the config, not from this struct.
type Action struct {
	InstanceID        string
	JobID             string
	Action            string
	TaskKey           string
	Status            int
	ProjectCode       string
	ProcessInstanceID string
	CreateTime        time.Time
	LastUpdate        time.Time
	RegionID          string
	Params            map[string]string
}

// New returns a Client for cfg.
func New(cfg *Config) *Client {
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// apiKey resolves the environment's API key: the env var named in the config
// wins, then the config default.
func (c *Client) apiKey(env *Environment) string {
	if env.APIKeyEnv != "" {
		if v := os.Getenv(env.APIKeyEnv); v != "" {
			return v
		}
	}
	return env.APIKeyDefault
}

// envForInstance routes an instance ID to the first environment whose prefix
// matches, falling back to the first configured environment.
func (c *Client) envForInstance(instanceID string) *Environment {
	for i := range c.cfg.Environments {
		env := &c.cfg.Environments[i]
		for _, prefix := range env.InstancePrefixes {
			if strings.HasPrefix(instanceID, prefix) {
				return env
			}
		}
	}
	return &c.cfg.Environments[0]
}

// ListActions fetches the action history for an instance over the last hours.
func (c *Client) ListActions(ctx context.Context, instanceID string, hours int) ([]Action, error) {
	if hours <= 0 {
		hours = 24
	}
	env := c.envForInstance(instanceID)
	to := time.Now()
	from := to.Add(-time.Duration(hours) * time.Hour)

	path := fmt.Sprintf("%s/api/v1/ops/resource/custInstance/%s/action/milvus/list",
		strings.TrimSuffix(env.BaseURL, "/"), instanceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey(env))
	q := req.URL.Query()
	q.Set("pageSize", strconv.Itoa(env.pageSize()))
	q.Set("createTimeFrom", strconv.FormatInt(from.UnixMilli(), 10))
	q.Set("createTimeTo", strconv.FormatInt(to.UnixMilli(), 10))
	req.URL.RawQuery = q.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list actions for %s: %w", instanceID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list actions for %s: status %d: %s", instanceID, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				InstanceID        string `json:"instanceId"`
				JobID             string `json:"jobId"`
				Action            string `json:"action"`
				TaskKey           string `json:"taskKey"`
				Status            int    `json:"status"`
				ProjectCode       string `json:"projectCode"`
				ProcessInstanceID string `json:"processInstanceId"`
				CreateTime        string `json:"createTime"`
				LastUpdate        string `json:"lastUpdate"`
				RegionID          string `json:"regionId"`
				Params            string `json:"params"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode actions for %s: %w", instanceID, err)
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("list actions for %s: code %d", instanceID, payload.Code)
	}

	actions := make([]Action, 0, len(payload.Data.List))
	for _, item := range payload.Data.List {
		actions = append(actions, Action{
			InstanceID:        item.InstanceID,
			JobID:             item.JobID,
			Action:            item.Action,
			TaskKey:           item.TaskKey,
			Status:            item.Status,
			ProjectCode:       item.ProjectCode,
			ProcessInstanceID: item.ProcessInstanceID,
			CreateTime:        parseTime(item.CreateTime),
			LastUpdate:        parseTime(item.LastUpdate),
			RegionID:          item.RegionID,
			Params:            parseParams(item.Params),
		})
	}
	return actions, nil
}

// parseTime parses the API's "2006-01-02 15:04:05" timestamps.
func parseTime(value string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", value, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseParams decodes the opaque params JSON blob into a string map.
func parseParams(raw string) map[string]string {
	out := make(map[string]string)
	if strings.TrimSpace(raw) == "" {
		return out
	}
	var nested map[string]any
	if err := json.Unmarshal([]byte(raw), &nested); err != nil {
		return out
	}
	for k, v := range nested {
		switch val := v.(type) {
		case string:
			out[k] = val
		case bool:
			out[k] = strconv.FormatBool(val)
		case float64:
			out[k] = strconv.FormatFloat(val, 'f', -1, 64)
		}
	}
	return out
}
