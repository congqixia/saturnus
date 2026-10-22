// Package cloudops provides a config-driven client for the cloud-ops console
// API. All environment-specific values (base URLs, link URL templates and the
// display mapping) live in a JSON config file that is intentionally kept out of
// the repository (see cloudops.example.json), so committing the source does not
// leak internal endpoints or credentials.
package cloudops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"
)

// Config is the root of the cloudops JSON config file.
type Config struct {
	Environments []Environment     `json:"environments"`
	Links        LinkTemplates     `json:"links"`
	Display      DisplayConfig     `json:"display"`
	StatusLabels map[string]string `json:"status_labels"`
}

// Environment describes one cloud-ops API endpoint (e.g. overseas/domestic
// production). Instances are routed to an environment by ID prefix.
type Environment struct {
	Name             string   `json:"name"`
	BaseURL          string   `json:"base_url"`
	APIKeyEnv        string   `json:"api_key_env"`
	APIKeyDefault    string   `json:"api_key_default"`
	InstancePrefixes []string `json:"instance_prefixes"`
	PageSize         int      `json:"page_size"`
	Timeout          string   `json:"timeout"`
}

// LinkTemplates are URL templates with {placeholder} tokens that get resolved
// against each action's fields at render time.
type LinkTemplates struct {
	Dolphin string `json:"dolphin"`
	Grafana string `json:"grafana"`
	OpsLog  string `json:"ops_log"`
}

// DisplayConfig controls how actions are rendered in the reply table.
type DisplayConfig struct {
	Title   string         `json:"title"`
	Columns []ColumnConfig `json:"columns"`
}

// ColumnConfig maps a column key to a display label, cell type and width.
type ColumnConfig struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"`
	Width string `json:"width"`
}

// Load reads and validates a cloudops JSON config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Environments) == 0 {
		return nil, errors.New("cloudops config: no environments")
	}
	return &cfg, nil
}

var placeholderRe = regexp.MustCompile(`\{[^{}]+\}`)

// subst replaces {key} placeholders in tmpl with values. Unknown placeholders
// resolve to the empty string so a missing optional field does not panic.
func subst(tmpl string, values map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		if v, ok := values[m[1:len(m)-1]]; ok {
			return v
		}
		return ""
	})
}

// timeout parses the configured per-request timeout.
func (e *Environment) timeout() time.Duration {
	if e.Timeout == "" {
		return 0
	}
	d, err := time.ParseDuration(e.Timeout)
	if err != nil {
		return 0
	}
	return d
}

// pageSize returns the configured page size, defaulting to 200.
func (e *Environment) pageSize() int {
	if e.PageSize > 0 {
		return e.PageSize
	}
	return 200
}
