package api

import (
	"encoding/json"
	"testing"
	"time"
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

// Prune operations can delete gigabytes of images and build cache, so they
// share the 5-minute budget with pull/run instead of the 30-second default;
// otherwise the HTTP call would 504 while the agent keeps deleting.
func TestDockerOpTimeout(t *testing.T) {
	for _, op := range []string{"pull", "run", "system_prune", "prune", "volume_prune"} {
		if got := dockerOpTimeout(op); got != 5*time.Minute {
			t.Errorf("%s 应为 5 分钟，实际 %v", op, got)
		}
	}
	for _, op := range []string{"ps", "images", "system_df", "start", "rm", "bogus"} {
		if got := dockerOpTimeout(op); got != 30*time.Second {
			t.Errorf("%s 应为 30 秒，实际 %v", op, got)
		}
	}
}

// ps、ps_all 与 images 即使只有 0 或 1 条记录，也必须返回数组（而非单个对象），
// 否则前端按数组遍历判断镜像使用状态时会在单容器宿主机上报错失效。
func TestFormatDockerPayloadListOpsAlwaysReturnSlice(t *testing.T) {
	cases := []struct {
		payload string
		wantLen int
	}{
		{"", 0},
		{`{"ID":"only1"}` + "\n", 1},
		{`{"ID":"a"}` + "\n" + `{"ID":"b"}` + "\n", 2},
	}
	for _, op := range []string{"ps", "ps_all", "images"} {
		for _, tc := range cases {
			res := formatDockerPayload(op, []byte(tc.payload))
			rows, ok := res["data"].([]any)
			if !ok {
				t.Fatalf("op=%s payload=%q: data 应为 []any，实际 %T", op, tc.payload, res["data"])
			}
			if len(rows) != tc.wantLen {
				t.Errorf("op=%s payload=%q: len(data)=%d, want %d", op, tc.payload, len(rows), tc.wantLen)
			}
		}
	}
	logs := formatDockerPayload("logs", []byte(`{"not":"json"}`+"\nline2"))
	if logs["raw"] == "" {
		t.Errorf("logs 应保留 raw 文本，实际 %+v", logs)
	}
}
