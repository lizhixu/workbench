package release

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubAsset describes a downloadable release asset from GitHub API.
type GitHubAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// GitHubRelease describes a release object returned by GitHub API.
type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Assets      []GitHubAsset `json:"assets"`
}

// UpdateInfo carries the latest release status compared to the running server.
type UpdateInfo struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	HasUpdate      bool   `json:"has_update"`
	IsBeta         bool   `json:"is_beta"`
	ReleaseNotes   string `json:"release_notes"`
	PublishedAt    string `json:"published_at"`
	AssetURL       string `json:"asset_url"`
	AssetName      string `json:"asset_name"`
	AssetSize      int64  `json:"asset_size"`
	ChecksumsURL   string `json:"checksums_url"`
	CheckedAt      string `json:"checked_at"`
	Channel        string `json:"channel"` // "beta" or "stable"
}

var (
	cacheMu     sync.Mutex
	cachedInfo  *UpdateInfo
	cachedAt    time.Time
	cachedBeta  bool
	cachedCurV  string
)

// ClearUpdateCache clears the in-memory update check cache.
func ClearUpdateCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cachedInfo = nil
}

// CheckUpdate queries GitHub Releases for the latest version matching the selected channel.
// Results are cached for 5 minutes unless force is true.
func CheckUpdate(ctx context.Context, currentVersion string, joinBeta bool, force bool) (*UpdateInfo, error) {
	cacheMu.Lock()
	if !force && cachedInfo != nil && cachedBeta == joinBeta && cachedCurV == currentVersion && time.Since(cachedAt) < 5*time.Minute {
		res := *cachedInfo
		cacheMu.Unlock()
		return &res, nil
	}
	cacheMu.Unlock()

	repo := os.Getenv("WATCHMAN_REPO")
	if repo == "" {
		repo = "lizhixu/workbench"
	}

	channel := "stable"
	if joinBeta {
		channel = "beta"
	}

	client := &http.Client{Timeout: 12 * time.Second}

	var targetRelease *GitHubRelease

	if joinBeta {
		// Beta channel: fetch latest releases (including pre-releases) and pick the first non-draft.
		apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=5", repo)
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github.v3+json")
		req.Header.Set("User-Agent", "Watchman-Server/"+currentVersion)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("请求 GitHub API 失败: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub API 返回状态码 %d", resp.StatusCode)
		}

		var releases []GitHubRelease
		if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
			return nil, fmt.Errorf("解析 GitHub Releases JSON 失败: %w", err)
		}

		for i := range releases {
			if !releases[i].Draft {
				targetRelease = &releases[i]
				break
			}
		}
	} else {
		// Stable channel: fetch /releases/latest which GitHub resolves to the latest non-prerelease.
		apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github.v3+json")
		req.Header.Set("User-Agent", "Watchman-Server/"+currentVersion)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("请求 GitHub API 失败: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var rel GitHubRelease
			if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
				return nil, fmt.Errorf("解析 GitHub Release JSON 失败: %w", err)
			}
			targetRelease = &rel
		} else if resp.StatusCode == http.StatusNotFound {
			// GitHub returns 404 when there is no stable (non-prerelease) release yet.
			// Return clean info informing user about stable availability.
			info := &UpdateInfo{
				CurrentVersion: currentVersion,
				LatestVersion:  currentVersion,
				HasUpdate:      false,
				IsBeta:         false,
				ReleaseNotes:   "官方仓库暂无正式版发布。如需体验最新功能，可开启「加入测试计划」获取预发布版本。",
				PublishedAt:    "",
				CheckedAt:      time.Now().Format(time.RFC3339),
				Channel:        channel,
			}
			cacheMu.Lock()
			cachedInfo = info
			cachedAt = time.Now()
			cachedBeta = joinBeta
			cachedCurV = currentVersion
			cacheMu.Unlock()
			return info, nil
		} else {
			return nil, fmt.Errorf("GitHub API 返回状态码 %d", resp.StatusCode)
		}
	}

	if targetRelease == nil {
		return &UpdateInfo{
			CurrentVersion: currentVersion,
			LatestVersion:  currentVersion,
			HasUpdate:      false,
			IsBeta:         false,
			ReleaseNotes:   "未检索到可用发布版本",
			CheckedAt:      time.Now().Format(time.RFC3339),
			Channel:        channel,
		}, nil
	}

	// Match target dist asset: watchman-dist-{TAG}-linux-{ARCH}.tar.gz
	tag := targetRelease.TagName
	expectedAsset := fmt.Sprintf("watchman-dist-%s-%s-%s.tar.gz", tag, runtime.GOOS, runtime.GOARCH)

	var (
		matchedAssetURL  string
		matchedAssetName string
		matchedAssetSize int64
		checksumsURL     string
	)

	mirror := os.Getenv("GITHUB_MIRROR")
	if mirror == "" {
		mirror = os.Getenv("WATCHMAN_GITHUB_MIRROR")
	}
	mirror = strings.TrimSuffix(strings.TrimSpace(mirror), "/")

	for _, a := range targetRelease.Assets {
		if a.Name == expectedAsset {
			matchedAssetName = a.Name
			matchedAssetSize = a.Size
			matchedAssetURL = a.BrowserDownloadURL
			if mirror != "" && strings.HasPrefix(matchedAssetURL, "https://github.com/") {
				matchedAssetURL = mirror + "/" + matchedAssetURL
			}
		}
		if a.Name == "CHECKSUMS.txt" {
			checksumsURL = a.BrowserDownloadURL
			if mirror != "" && strings.HasPrefix(checksumsURL, "https://github.com/") {
				checksumsURL = mirror + "/" + checksumsURL
			}
		}
	}

	// Compare current version with latest release version.
	cmp := CompareVersions(currentVersion, tag)
	hasUpdate := cmp < 0

	info := &UpdateInfo{
		CurrentVersion: currentVersion,
		LatestVersion:  tag,
		HasUpdate:      hasUpdate,
		IsBeta:         targetRelease.Prerelease,
		ReleaseNotes:   targetRelease.Body,
		PublishedAt:    targetRelease.PublishedAt,
		AssetURL:       matchedAssetURL,
		AssetName:      matchedAssetName,
		AssetSize:      matchedAssetSize,
		ChecksumsURL:   checksumsURL,
		CheckedAt:      time.Now().Format(time.RFC3339),
		Channel:        channel,
	}

	cacheMu.Lock()
	cachedInfo = info
	cachedAt = time.Now()
	cachedBeta = joinBeta
	cachedCurV = currentVersion
	cacheMu.Unlock()

	return info, nil
}

// CompareVersions compares two semver strings (e.g. "v0.1.0-beta.6" and "v0.1.0-beta.7").
// Returns -1 if v1 < v2, 0 if v1 == v2, and 1 if v1 > v2.
// "0.1.0-dev" or empty is treated as older than any tagged release.
func CompareVersions(v1, v2 string) int {
	v1 = strings.TrimPrefix(strings.TrimSpace(v1), "v")
	v2 = strings.TrimPrefix(strings.TrimSpace(v2), "v")
	if v1 == v2 {
		return 0
	}
	if v1 == "" || strings.HasSuffix(v1, "-dev") {
		return -1
	}
	if v2 == "" || strings.HasSuffix(v2, "-dev") {
		return 1
	}

	core1, pre1 := splitCoreAndPre(v1)
	core2, pre2 := splitCoreAndPre(v2)

	nums1 := parseNumericParts(core1)
	nums2 := parseNumericParts(core2)
	maxLen := len(nums1)
	if len(nums2) > maxLen {
		maxLen = len(nums2)
	}
	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(nums1) {
			n1 = nums1[i]
		}
		if i < len(nums2) {
			n2 = nums2[i]
		}
		if n1 < n2 {
			return -1
		}
		if n1 > n2 {
			return 1
		}
	}

	// If core versions are identical:
	// A standard release has higher precedence than a pre-release (semver spec 11.4).
	if pre1 == "" && pre2 != "" {
		return 1 // e.g. 0.1.0 > 0.1.0-beta.7
	}
	if pre1 != "" && pre2 == "" {
		return -1 // e.g. 0.1.0-beta.7 < 0.1.0
	}
	if pre1 == "" && pre2 == "" {
		return 0
	}

	// Compare pre-release components dot by dot (e.g. "beta.6" vs "beta.7")
	parts1 := strings.Split(pre1, ".")
	parts2 := strings.Split(pre2, ".")
	pLen := len(parts1)
	if len(parts2) > pLen {
		pLen = len(parts2)
	}
	for i := 0; i < pLen; i++ {
		if i >= len(parts1) {
			return -1 // fewer parts is smaller (e.g. beta < beta.1)
		}
		if i >= len(parts2) {
			return 1
		}
		p1, p2 := parts1[i], parts2[i]
		if p1 == p2 {
			continue
		}
		num1, err1 := strconv.Atoi(p1)
		num2, err2 := strconv.Atoi(p2)
		if err1 == nil && err2 == nil {
			if num1 < num2 {
				return -1
			}
			if num1 > num2 {
				return 1
			}
		} else {
			if p1 < p2 {
				return -1
			}
			return 1
		}
	}

	return 0
}

func splitCoreAndPre(v string) (string, string) {
	if idx := strings.IndexByte(v, '-'); idx >= 0 {
		return v[:idx], v[idx+1:]
	}
	return v, ""
}

func parseNumericParts(core string) []int {
	parts := strings.Split(core, ".")
	res := make([]int, 0, len(parts))
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		res = append(res, n)
	}
	return res
}
