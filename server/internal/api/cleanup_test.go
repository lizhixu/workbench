package api

import "testing"

// validCleanupCategories 是 systemCleanup 的服务端白名单校验，与 Agent
// 侧同款；空/未知/重复一律在下发前拒绝。
func TestValidCleanupCategories(t *testing.T) {
	if err := validCleanupCategories([]string{"apt_cache", "journal", "tmp_stale", "logs_old"}); err != nil {
		t.Errorf("全选应通过，实际 %v", err)
	}
	if err := validCleanupCategories([]string{"journal"}); err != nil {
		t.Errorf("单选应通过，实际 %v", err)
	}
	cases := []struct {
		name string
		cats []string
	}{
		{"空", []string{}},
		{"nil", nil},
		{"未知", []string{"autoremove"}},
		{"未知混在已知里", []string{"apt_cache", "rm_rf"}},
		{"重复", []string{"journal", "journal"}},
	}
	for _, tc := range cases {
		if err := validCleanupCategories(tc.cats); err == nil {
			t.Errorf("%s 应拒绝：%v", tc.name, tc.cats)
		}
	}
}
