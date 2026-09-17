// Native Feishu card tables.
//
// Feishu renders a real table only through the card JSON 2.0 `table` component
// (declared via "schema": "2.0"). An earlier attempt wrapped the same rows in a
// fenced code block inside a lark_md div, but that rendered as raw "|" text
// instead of a table, so do not regress to a text/code-block approach.
//
// Reference docs (verified 2026-09):
//   - Card JSON 2.0 structure: https://open.feishu.cn/document/feishu-cards/card-json-v2-structure
//   - Table component:         https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/table
//
// Key constraints learned from the docs:
//   - Requires Feishu client >= v7.20; older clients render an upgrade prompt
//     instead of the table body.
//   - The table component must sit at the card root (it cannot be nested inside
//     other components); max 5 tables per card, max 50 columns per table.
//   - columns[].width: "auto" | fixed px in [80,600] | percent in [1,100].
//     Percent is relative to the table canvas (card width minus padding). Keep
//     each table's per-column percentages summing to exactly 100% so the table
//     fills the card and does not overflow into a horizontal scrollbar; plain
//     "auto" widths tend to overflow the canvas.
//   - config.width_mode: "default" (600px cap on PC/iPad) | "compact" (400px) |
//     "fill" (fills the chat window width). We use "fill" to maximize width.
//     Note: the 1.0 "wide_screen_mode" config key is ignored under schema 2.0.
//   - columns[].data_type: text | lark_md | number | options | persons | date | markdown.
//     date needs a Unix millisecond timestamp and Feishu renders it in the
//     user's local timezone. options renders colored tags via
//     {"text": <label>, "color": <name>}.
//   - page_size controls rows per page, integer in [1,10].
//   - Cells that do not fit their column are truncated with an ellipsis;
//     users can hover/click a truncated cell to see the full value.
package lark

import "time"

// Table column data types for native Feishu card 2.0 tables.
const (
	ColumnText     = "text"
	ColumnMD       = "lark_md"
	ColumnNumber   = "number"
	ColumnOption   = "options"
	ColumnPerson   = "persons"
	ColumnDate     = "date"
	ColumnMarkdown = "markdown"
)

// TableColumn describes one column of a native card table.
type TableColumn struct {
	Name       string
	Display    string
	DataType   string
	Width      string
	DateFormat string
}

// TableCard is a native Feishu card 2.0 table. Rows map column names to
// cell values (string, number, or option/person tag objects).
type TableCard struct {
	Title   string
	Columns []TableColumn
	Rows    []map[string]any
}

// OptionTag builds an option-tag cell value for an "options" column.
func OptionTag(text, color string) map[string]any {
	if color == "" {
		return map[string]any{"text": text}
	}
	return map[string]any{"text": text, "color": color}
}

// UnixMillis returns the millisecond timestamp expected by "date" columns,
// or zero for a zero time.
func UnixMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// SendTable sends an interactive card containing a native table. The card
// uses the JSON 2.0 schema, so clients below v7.20 will show an upgrade
// prompt instead of the table.
func (c *Client) SendTable(receiveIDType, receiveID string, card TableCard) error {
	if !c.Enabled() {
		return nil
	}
	payload := map[string]any{
		"receive_id": receiveID,
		"msg_type":   "interactive",
		"content":    mustMarshal(buildTableCard(card)),
	}
	url := "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=" + receiveIDType
	return c.postJSON(url, payload)
}

func buildTableCard(card TableCard) map[string]any {
	columns := make([]map[string]any, 0, len(card.Columns))
	for _, col := range card.Columns {
		column := map[string]any{
			"name":         col.Name,
			"display_name": col.Display,
			"data_type":    col.DataType,
		}
		if col.Width != "" {
			column["width"] = col.Width
		}
		if col.DateFormat != "" {
			column["date_format"] = col.DateFormat
		}
		columns = append(columns, column)
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "fill"},
		"header": map[string]any{
			"template": "blue",
			"title":    map[string]any{"tag": "plain_text", "content": card.Title},
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{
					"tag":       "table",
					"page_size": 10,
					"columns":   columns,
					"rows":      card.Rows,
				},
			},
		},
	}
}
