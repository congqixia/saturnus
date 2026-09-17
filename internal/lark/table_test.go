package lark

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildTableCard(t *testing.T) {
	card := buildTableCard(TableCard{
		Title: "PR Reviews",
		Columns: []TableColumn{
			{Name: "id", Display: "ID", DataType: ColumnText, Width: "20%"},
			{Name: "status", Display: "Status", DataType: ColumnOption, Width: "12%"},
			{Name: "created", Display: "Created", DataType: ColumnDate, DateFormat: "YYYY-MM-DD HH:mm", Width: "17%"},
		},
		Rows: []map[string]any{
			{
				"id":      "rvw_1",
				"status":  OptionTag("failed", "red"),
				"created": int64(1699341315000),
			},
		},
	})

	if card["schema"] != "2.0" {
		t.Fatalf("expected schema 2.0, got %v", card["schema"])
	}

	config, _ := card["config"].(map[string]any)
	if config["width_mode"] != "fill" {
		t.Fatalf("expected width_mode fill, got %v", config["width_mode"])
	}

	header, _ := card["header"].(map[string]any)
	if header["title"].(map[string]any)["content"] != "PR Reviews" {
		t.Fatalf("unexpected header title: %v", header["title"])
	}

	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]map[string]any)
	if len(elements) != 1 {
		t.Fatalf("expected 1 element, got %d", len(elements))
	}
	table := elements[0]
	if table["tag"] != "table" {
		t.Fatalf("expected table element, got %v", table["tag"])
	}
	if table["page_size"] != 10 {
		t.Fatalf("expected page_size 10, got %v", table["page_size"])
	}

	columns, _ := table["columns"].([]map[string]any)
	if len(columns) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(columns))
	}
	if columns[0]["name"] != "id" || columns[0]["display_name"] != "ID" || columns[0]["width"] != "20%" {
		t.Fatalf("unexpected first column: %v", columns[0])
	}
	if columns[1]["data_type"] != "options" {
		t.Fatalf("expected options data_type, got %v", columns[1]["data_type"])
	}
	if columns[2]["data_type"] != "date" || columns[2]["date_format"] != "YYYY-MM-DD HH:mm" {
		t.Fatalf("unexpected date column: %v", columns[2])
	}

	rows, _ := table["rows"].([]map[string]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	status := rows[0]["status"].(map[string]any)
	if status["text"] != "failed" || status["color"] != "red" {
		t.Fatalf("unexpected status cell: %v", rows[0]["status"])
	}
}

func TestSendTablePayloadIsValidJSONCard(t *testing.T) {
	payload := map[string]any{
		"receive_id": "oc_1",
		"msg_type":   "interactive",
		"content":    mustMarshal(buildTableCard(TableCard{Title: "T", Columns: []TableColumn{{Name: "c", Display: "C", DataType: ColumnText}}, Rows: []map[string]any{{"c": "x"}}})),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if !strings.Contains(string(raw), `\"schema\":\"2.0\"`) {
		t.Fatalf("payload missing schema 2.0: %s", raw)
	}
}
