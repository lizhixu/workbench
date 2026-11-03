package apps

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// ---- 上传备份文件的自动识别（upload-scan）----
//
// 操作员把 docker cp 导出的裸 tar 或本系统导出的多卷快照上传后，往往说得出
// 「这是哪个应用的数据」，却分不清每份文件该进哪个挂载卷。识别不靠文件名
// 猜测，而是解开归档读内容：本系统快照内嵌 manifest.json（精确到卷）；
// 裸 tar 按顶层目录名与各卷的容器路径/基名匹配。识别结果只作为建议回显，
// 覆盖式恢复的最终目标卷仍由操作员在确认页过目（或手动改选），且永远不
// 会命中应用声明/探测卷白名单之外的目标。

const (
	// volumeScanEntryCap bounds how many tar headers one scan reads: it is a
	// completeness guard against a crafted archive stalling the panel, not a
	// correctness limit (directory-shape signals stabilize long before it).
	volumeScanEntryCap = 200000

	// volumeScanDirCap bounds the distinct directory-prefix set; matching only
	// needs path shape, which a real volume produces within a few thousand
	// distinct prefixes.
	volumeScanDirCap = 8192

	// volumeScanManifestCap bounds the manifest.json bytes parsed from a
	// snapshot tar. The panel writes manifests of a few KB; anything larger
	// is not ours.
	volumeScanManifestCap = 1 << 20

	// volumeScanTimeBudget bounds one file's scan wall clock.
	volumeScanTimeBudget = 30 * time.Second
)

// Scan match kinds, surfaced to the UI verbatim.
const (
	scanMatchManifest  = "manifest"  // snapshot tar: manifest.json mapped to live volumes
	scanMatchUnique    = "unique"    // exactly one candidate volume matched
	scanMatchAmbiguous = "ambiguous"  // several candidates matched: operator must pick
	scanMatchNone      = "none"      // nothing matched: operator must pick
)

// volumeScanMatch is one proposed (uploaded file → volume) pairing.
type volumeScanMatch struct {
	VolumeSpec    string `json:"volume_spec"`
	ContainerPath string `json:"container_path,omitempty"`
	HostPath      string `json:"host_path,omitempty"`
	Service       string `json:"service,omitempty"`
	VolumeType    string `json:"volume_type,omitempty"`
	// Reason tells the operator why this volume was proposed:
	// "manifest"（快照清单） | "container_path"（内容路径命中） | "basename"（目录名命中）.
	Reason string `json:"reason"`
}

// volumeScanCandidate is one live target volume of the app, in the same
// normalized shape a commit will validate against.
type volumeScanCandidate struct {
	Spec          string
	ContainerPath string
	HostPath      string // bind source path; empty for named volumes until commit resolves them
	Service       string
	VolumeType    string
}

// tarScanResult is what reading one staged archive yielded.
type tarScanResult struct {
	manifest *SnapshotManifest
	prefixes map[string]bool // distinct path prefixes up to depth 3 + top-level file names
	topDirs  []string        // ordered distinct top-level entry names, display-capped
	topSet   map[string]bool
}

// scanUploadTar reads a staged archive's structure: gzip is auto-detected by
// magic bytes (uploads accept both .tar and .tar.gz), entry names feed the
// matching prefix set, and a root manifest.json is parsed in full. It streams
// headers only — archive data blocks are skipped, so scanning stays cheap even
// for multi-GB uploads.
func scanUploadTar(file string) (*tarScanResult, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := make([]byte, 2)
	if _, err := io.ReadFull(f, br); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errors.New("文件为空或过小，不是有效的 tar 归档")
		}
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var src io.Reader = f
	if br[0] == 0x1f && br[1] == 0x8b {
		zr, zerr := gzip.NewReader(f)
		if zerr != nil {
			return nil, errors.New("gzip 解压失败，不是有效的 tar.gz 归档")
		}
		defer zr.Close()
		src = zr
	}

	res := &tarScanResult{prefixes: make(map[string]bool), topSet: make(map[string]bool)}
	deadline := time.Now().Add(volumeScanTimeBudget)
	tr := tar.NewReader(src)
	manifestSeen := false
	for i := 0; ; i++ {
		if i >= volumeScanEntryCap {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		hd, herr := tr.Next()
		if herr == io.EOF {
			break
		}
		if herr != nil {
			return nil, errors.New("tar 归档内容不完整或已损坏")
		}
		name := strings.TrimPrefix(hd.Name, "./")
		if name == "" || name == "." || name == "/" {
			continue
		}
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			continue
		}
		if !manifestSeen && name == "manifest.json" && hd.Typeflag != tar.TypeDir {
			raw, _ := io.ReadAll(io.LimitReader(tr, volumeScanManifestCap))
			var mf SnapshotManifest
			if json.Unmarshal(raw, &mf) == nil && len(mf.Volumes) > 0 {
				res.manifest = &mf
			}
			manifestSeen = true
		}
		segments := strings.Split(name, "/")
		if !res.topSet[segments[0]] {
			res.topSet[segments[0]] = true
			if len(res.topDirs) < 20 {
				res.topDirs = append(res.topDirs, segments[0])
			}
		}
		if len(res.prefixes) < volumeScanDirCap {
			res.prefixes[segments[0]] = true
			if len(segments) >= 2 {
				res.prefixes[segments[0]+"/"+segments[1]] = true
			}
			if len(segments) >= 3 {
				res.prefixes[segments[0]+"/"+segments[1]+"/"+segments[2]] = true
			}
		}
	}
	return res, nil
}

// volumeScanCandidates collects the app's live target volumes: declared
// volumes for single-container apps (parseable offline), discovered mounts for
// Compose apps (needs the agent online; offline degrades to no candidates and
// the UI falls back to manual selection with an offline hint).
func volumeScanCandidates(h *AppHandlers, app *Application) []volumeScanCandidate {
	if isComposeApp(app) {
		if h.appHub(app.HostID) == nil {
			return nil
		}
		disc, err := h.engine.DiscoverAppVolumes(app)
		if err != nil {
			return nil
		}
		cands := make([]volumeScanCandidate, 0, len(disc))
		for _, d := range disc {
			if d.ReadOnly {
				continue
			}
			cands = append(cands, volumeScanCandidate{
				Spec: d.VolumeSpec, ContainerPath: d.ContainerPath,
				HostPath: d.HostPath, Service: d.Service, VolumeType: d.VolumeType,
			})
		}
		return cands
	}
	cands := make([]volumeScanCandidate, 0, len(app.Volumes))
	for _, v := range app.Volumes {
		spec := strings.TrimSpace(v)
		if spec == "" {
			continue
		}
		src, dst, _, ok := parseVolumeSpec(spec)
		if !ok {
			continue
		}
		host := ""
		vType := "volume"
		if volumeIsBindMount(src) {
			host, vType = src, "bind"
		}
		cands = append(cands, volumeScanCandidate{Spec: spec, ContainerPath: dst, HostPath: host, VolumeType: vType})
	}
	return cands
}

// matchScanVolumes proposes which live volumes one uploaded file belongs to.
// A parsed manifest wins outright (exact per-volume mapping); a raw tar is
// scored by content shape: an entry path equal to (or nested under) the
// container path is strong, a top-level directory named like the container or
// host path basename is weaker. All positive matches are returned so the UI
// can preselect the unique one or let the operator disambiguate.
func matchScanVolumes(res *tarScanResult, cands []volumeScanCandidate) (match string, matched []volumeScanMatch) {
	if res.manifest != nil {
		for _, it := range res.manifest.Volumes {
			for _, c := range cands {
				if c.Spec == it.VolumeSpec || (it.ContainerPath != "" && c.ContainerPath == it.ContainerPath) {
					matched = append(matched, volumeScanMatch{
						VolumeSpec: c.Spec, ContainerPath: c.ContainerPath,
						HostPath: c.HostPath, Service: c.Service, VolumeType: c.VolumeType,
						Reason: "manifest",
					})
					break
				}
			}
		}
		if len(matched) > 0 {
			return scanMatchManifest, matched
		}
		return scanMatchNone, matched
	}

	type scored struct {
		m     volumeScanMatch
		score int
	}
	var hits []scored
	for _, c := range cands {
		m := volumeScanMatch{
			VolumeSpec: c.Spec, ContainerPath: c.ContainerPath,
			HostPath: c.HostPath, Service: c.Service, VolumeType: c.VolumeType,
		}
		score := 0
		if container := strings.TrimPrefix(c.ContainerPath, "/"); container != "" {
			if res.prefixes[container] {
				score, m.Reason = 3, "container_path"
			}
		}
		if score == 0 {
			if base := path.Base(c.ContainerPath); base != "" && base != "/" && base != "." && res.topSet[base] {
				score, m.Reason = 2, "basename"
			}
		}
		if score == 0 && c.HostPath != "" && c.VolumeType == "bind" {
			if base := path.Base(c.HostPath); base != "" && base != "/" && base != "." && res.topSet[base] {
				score, m.Reason = 1, "basename"
			}
		}
		if score > 0 {
			hits = append(hits, scored{m: m, score: score})
		}
	}
	if len(hits) == 0 {
		return scanMatchNone, matched
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	for _, h := range hits {
		matched = append(matched, h.m)
	}
	if len(matched) == 1 {
		return scanMatchUnique, matched
	}
	return scanMatchAmbiguous, matched
}

// resolveScanCandidates maps one commit pair's requested specs onto live
// candidate volumes, normalizing container-path-only picks to the current spec
// string and rejecting anything outside the whitelist. Deduping keys on the
// resolved candidate, so a pair that names the same volume twice by spec and
// by container path is still caught.
func resolveScanCandidates(specs []string, cands []volumeScanCandidate) ([]volumeScanCandidate, error) {
	seen := make(map[string]bool, len(specs))
	out := make([]volumeScanCandidate, 0, len(specs))
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, errors.New("挂载卷选择为空")
		}
		var found *volumeScanCandidate
		for i := range cands {
			if cands[i].Spec == s || cands[i].ContainerPath == s {
				found = &cands[i]
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("只能恢复到应用已声明或已挂载的卷: %s", s)
		}
		if seen[found.Spec] {
			return nil, fmt.Errorf("同一挂载卷在一次导入中只能出现一次: %s", found.Spec)
		}
		seen[found.Spec] = true
		out = append(out, *found)
	}
	return out, nil
}