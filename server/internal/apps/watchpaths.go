package apps

import (
	"fmt"
	"strings"
)

// NormalizeWatchPaths cleans a watch-paths list from an API request: trims
// whitespace and slashes, collapses duplicate slashes, drops empty and
// duplicate entries. Entries containing ".." segments or backslashes are
// rejected: Git paths are slash-separated and must stay inside the
// repository.
func NormalizeWatchPaths(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range in {
		p, err := normalizeWatchPath(raw)
		if err != nil {
			return nil, err
		}
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

func normalizeWatchPath(raw string) (string, error) {
	if strings.Contains(raw, "\\") {
		return "", fmt.Errorf("监听目录不能包含反斜杠: %q", raw)
	}
	p := cleanRepoPath(raw)
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("监听目录不能包含 ..: %q", raw)
		}
	}
	return p, nil
}

// cleanRepoPath normalizes a repo-relative path: trims whitespace, drops
// empty and "." segments, joins with single slashes. It never errors; use
// normalizeWatchPath when ".." must also be rejected.
func cleanRepoPath(raw string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(raw), func(r rune) bool { return r == '/' })
	var kept []string
	for _, seg := range parts {
		if seg == "." || seg == "" {
			continue
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "/")
}

// WatchPathsMatch reports whether any changed repo-relative path falls
// under the watch list: a changed path matches when it equals a watch
// entry (exact file) or lives under it (directory prefix). An empty watch
// list matches everything.
func WatchPathsMatch(watch, changed []string) bool {
	if len(watch) == 0 {
		return true
	}
	for _, c := range changed {
		p := cleanRepoPath(c)
		if p == "" {
			continue
		}
		for _, w := range watch {
			w = cleanRepoPath(w)
			if w == "" {
				continue
			}
			if p == w || strings.HasPrefix(p, w+"/") {
				return true
			}
		}
	}
	return false
}
