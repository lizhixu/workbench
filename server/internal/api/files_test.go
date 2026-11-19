package api

import (
	"net/url"
	"testing"
)

// parseContentRange backs GET /hosts/:id/files/content (the ranged read the
// file editor uses to sniff text vs binary before opening a file).
func TestParseContentRange(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantOffset int64
		wantLength int64
		wantErr    bool
	}{
		{"缺省", "", 0, contentSniffBytes, false},
		{"显式范围", "offset=100&length=512", 100, 512, false},
		{"length=0", "length=0", 0, 0, false},
		{"length 超上限钳制", "length=2097152", 0, contentRangeMax, false},
		{"offset 为负", "offset=-1", 0, 0, true},
		{"length 为负", "length=-5", 0, 0, true},
		{"offset 非数字", "offset=abc", 0, 0, true},
		{"length 非数字", "length=8k", 0, 0, true},
	}
	for _, tc := range cases {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatalf("%s: 解析 query 失败 %v", tc.name, err)
		}
		offset, length, err := parseContentRange(q)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: 期望报错，实际 offset=%d length=%d", tc.name, offset, length)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 不应报错，实际 %v", tc.name, err)
			continue
		}
		if offset != tc.wantOffset || length != tc.wantLength {
			t.Errorf("%s: 得到 offset=%d length=%d，期望 offset=%d length=%d",
				tc.name, offset, length, tc.wantOffset, tc.wantLength)
		}
	}
}
