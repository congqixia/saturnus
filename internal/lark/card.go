package lark

// MarkdownCard is a native Feishu card 2.0 whose body is a single "markdown"
// element, used for free-form detail views that are not tabular (e.g.
// /sat describe review <id>).
//
// Docs: https://open.feishu.cn/document/feishu-cards/card-json-v2-components/content-components/rich-text
type MarkdownCard struct {
	Title   string
	Content string
}

// SendMarkdownCard sends an interactive card whose body is a markdown element.
// Like the native tables in table.go, this uses the JSON 2.0 schema, so clients
// below v7.20 will show an upgrade prompt instead of the card body.
func (c *Client) SendMarkdownCard(receiveIDType, receiveID string, card MarkdownCard) error {
	if !c.Enabled() {
		return nil
	}
	payload := map[string]any{
		"receive_id": receiveID,
		"msg_type":   "interactive",
		"content":    mustMarshal(buildMarkdownCard(card)),
	}
	url := "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type=" + receiveIDType
	return c.postJSON(url, payload)
}

func buildMarkdownCard(card MarkdownCard) map[string]any {
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{"width_mode": "fill"},
		"header": map[string]any{
			"template": "blue",
			"title":    map[string]any{"tag": "plain_text", "content": card.Title},
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{"tag": "markdown", "content": card.Content},
			},
		},
	}
}
