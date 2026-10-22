package cloudops

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"saturnus/internal/lark"
)

// Reply renders a list of actions into a native Feishu table card plus a plain
// text fallback. The columns, labels, cell types and link URLs all come from
// the config so no internal info is hardcoded in source.
func (c *Client) Reply(instanceID string, hours int, actions []Action) (*lark.TableCard, string) {
	cfg := c.cfg.Display
	if len(actions) == 0 {
		return nil, fmt.Sprintf("No actions for %s in the last %dh.", instanceID, hours)
	}

	columns := make([]lark.TableColumn, 0, len(cfg.Columns))
	for _, col := range cfg.Columns {
		columns = append(columns, lark.TableColumn{
			Name:     col.Key,
			Display:  col.Label,
			DataType: cellDataType(col.Type),
			Width:    col.Width,
		})
	}

	var b strings.Builder
	rows := make([]map[string]any, 0, len(actions))
	for _, action := range actions {
		row := make(map[string]any, len(cfg.Columns))
		for _, col := range cfg.Columns {
			row[col.Key] = c.cell(action, col)
		}
		rows = append(rows, row)
		fmt.Fprintf(&b, "%s %s %s status=%s %s\n",
			formatActionTime(action.CreateTime), action.Action, c.change(action), c.statusLabel(action.Status), c.actionLink(action))
	}

	title := cfg.Title
	if title == "" {
		title = "Cluster Actions"
	}
	return &lark.TableCard{
		Title:   fmt.Sprintf("%s: %s", title, instanceID),
		Columns: columns,
		Rows:    rows,
	}, strings.TrimSpace(b.String())
}

func formatActionTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// cell produces the value for one config column of one action row.
func (c *Client) cell(action Action, col ColumnConfig) any {
	switch col.Key {
	case "createTime":
		return lark.UnixMillis(action.CreateTime)
	case "action":
		return action.Action
	case "taskKey":
		return action.TaskKey
	case "status":
		return lark.OptionTag(c.statusLabel(action.Status), statusColor(action.Status))
	case "change":
		return c.change(action)
	case "jobId":
		return action.JobID
	case "processInstanceId":
		return action.ProcessInstanceID
	case "regionId":
		return action.RegionID
	case "instanceId":
		return action.InstanceID
	case "links":
		return c.actionLink(action)
	}
	if v, ok := action.Params[col.Key]; ok {
		return v
	}
	return ""
}

// cellDataType maps a config column type to a lark table cell data type.
func cellDataType(kind string) string {
	switch kind {
	case "date":
		return lark.ColumnDate
	case "options":
		return lark.ColumnOption
	case "markdown", "md", "lark_md":
		return lark.ColumnMD
	case "number":
		return lark.ColumnNumber
	default:
		return lark.ColumnText
	}
}

// change renders a human-readable summary of a scale operation from the
// params blob (e.g. "replica 5→9" or "class-192-5→192-9").
func (c *Client) change(action Action) string {
	source := action.Params["sourceReplica"]
	replica := action.Params["replica"]
	if source != "" && replica != "" && source != replica {
		return fmt.Sprintf("replica %s→%s", source, replica)
	}
	oldClass := action.Params["oldClassId"]
	newClass := action.Params["classId"]
	if oldClass != "" && newClass != "" && oldClass != newClass {
		return fmt.Sprintf("class %s→%s", shortClass(oldClass), shortClass(newClass))
	}
	if downgrade := action.Params["downgrade"]; downgrade == "true" || downgrade == "false" {
		verb := "scale up"
		if downgrade == "true" {
			verb = "scale down"
		}
		return verb
	}
	return action.Action
}

// shortClass strips the "class-" prefix from a class id for display.
func shortClass(id string) string {
	return strings.TrimPrefix(id, "class-")
}

// statusLabel maps an API status int to its label from config.
func (c *Client) statusLabel(status int) string {
	key := strconv.Itoa(status)
	if label, ok := c.cfg.StatusLabels[key]; ok {
		return label
	}
	return key
}

// statusColor maps an API status int to a Feishu tag color.
func statusColor(status int) string {
	switch status {
	case 0:
		return "grey"
	case 1:
		return "blue"
	case 2, 7:
		return "green"
	case 3, 4:
		return "red"
	case 5:
		return "grey"
	default:
		return ""
	}
}

// actionLink builds the config-driven link URLs (dolphin/grafana/ops) for one
// action as a single markdown cell.
func (c *Client) actionLink(action Action) string {
	values := map[string]string{
		"instanceId":        action.InstanceID,
		"jobId":             action.JobID,
		"action":            action.Action,
		"taskKey":           action.TaskKey,
		"status":            strconv.Itoa(action.Status),
		"projectCode":       action.ProjectCode,
		"processInstanceId": action.ProcessInstanceID,
		"regionId":          action.RegionID,
		"startTimeMs":       strconv.FormatInt(action.CreateTime.UnixMilli(), 10),
		"endTimeMs":         strconv.FormatInt(action.LastUpdate.UnixMilli(), 10),
		"startTimeSec":      strconv.FormatInt(action.CreateTime.Unix(), 10),
		"endTimeSec":        strconv.FormatInt(action.LastUpdate.Unix(), 10),
		"requestId":         action.Params["requestId"],
		"sourceReplica":     action.Params["sourceReplica"],
		"replica":           action.Params["replica"],
		"oldClassId":        action.Params["oldClassId"],
		"classId":           action.Params["classId"],
	}
	env := c.envForInstance(action.InstanceID)
	base := strings.TrimSuffix(env.BaseURL, "/")

	links := []string{}
	if tmpl := c.cfg.Links.Dolphin; tmpl != "" {
		links = append(links, fmt.Sprintf("[dolphin](%s)", subst(tmpl, merge(values, map[string]string{"base": base}))))
	}
	if tmpl := c.cfg.Links.Grafana; tmpl != "" {
		links = append(links, fmt.Sprintf("[grafana](%s)", subst(tmpl, merge(values, map[string]string{"base": base}))))
	}
	if tmpl := c.cfg.Links.OpsLog; tmpl != "" {
		links = append(links, fmt.Sprintf("[ops](%s)", subst(tmpl, merge(values, map[string]string{"base": base}))))
	}
	return strings.Join(links, " ")
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// SortedKeys returns config column keys in display order (used by tests).
func (c *Client) SortedKeys() []string {
	keys := make([]string, 0, len(c.cfg.Display.Columns))
	for _, col := range c.cfg.Display.Columns {
		keys = append(keys, col.Key)
	}
	sort.Strings(keys)
	return keys
}
