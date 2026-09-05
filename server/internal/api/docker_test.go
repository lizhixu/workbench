package api

import (
	"encoding/json"
	"testing"
)

// `docker ps --format {{json .}}` prints one object per line: zero containers is
// empty, one container is a standalone JSON object, and two or more are NDJSON.
// The list endpoints must return an array in all three shapes, otherwise the
// browser renders an empty table for a single container.
func TestParseNDJSONShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"空输出", "", 0},
		{"仅空行", "\n\n", 0},
		{"单个对象", `{"ID":"a1","Names":"one"}`, 1},
		{"单个对象带换行", `{"ID":"a1","Names":"one"}` + "\n", 1},
		{"两个对象", `{"ID":"a1"}` + "\n" + `{"ID":"b2"}`, 2},
		{"三个对象带空行", `{"ID":"a1"}` + "\n\n" + `{"ID":"b2"}` + "\n" + `{"ID":"c3"}` + "\n", 3},
	}
	for _, tc := range cases {
		rows := parseNDJSON([]byte(tc.raw))
		if rows == nil {
			rows = []any{}
		}
		if len(rows) != tc.want {
			t.Errorf("%s: 解析出 %d 条，期望 %d 条", tc.name, len(rows), tc.want)
		}
		// Whatever the input shape, the result must marshal as a JSON array so
		// the frontend's `data: any[]` contract holds.
		out, err := json.Marshal(rows)
		if err != nil {
			t.Fatalf("%s: 序列化失败 %v", tc.name, err)
		}
		if len(out) == 0 || out[0] != '[' {
			t.Errorf("%s: 结果必须是 JSON 数组，实际 %s", tc.name, out)
		}
	}
}

// Malformed output must not be mistaken for a list.
func TestParseNDJSONRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"not json", `{"ok":true}` + "\nnot json", "<html>error</html>"} {
		if rows := parseNDJSON([]byte(raw)); rows != nil {
			t.Errorf("非 JSON 输入 %q 不应被解析为列表，得到 %v", raw, rows)
		}
	}
}
