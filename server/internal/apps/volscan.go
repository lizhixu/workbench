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
	scanMatchAmbiguous = "ambiguous" // several candidates matched: operator must pick
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
	// "manifest"（快照清单） | "container_path"（内容路径命中） | "basename"（目录名命中） | "filename"（文件名特征命中） | "single_candidate"（应用唯一可用挂载卷）.
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
			top := segments[0]
			if !res.topSet[top] {
				res.topSet[top] = true
				if len(res.topDirs) < 20 {
					res.topDirs = append(res.topDirs, top)
				}
			}
			// If top has leading dots or underscores, e.g. ".data", also record "data"
			cleanTop := strings.TrimLeft(top, "._")
			if cleanTop != "" && cleanTop != top {
				res.topSet[cleanTop] = true
			}

			if len(res.prefixes) < volumeScanDirCap {
				res.prefixes[top] = true
				if cleanTop != "" {
					res.prefixes[cleanTop] = true
				}
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

	// cleanArchiveStem extracts the base name of an archive file without archive extensions
	// (.tar.gz, .tgz, .tar, etc.) and without outer dots/punctuation.
	// e.g. ".data.tar" -> "data", "pb_data.tar.gz" -> "pb_data", "backup-data.tar" -> "backup-data".
	func cleanArchiveStem(filename string) string {
		name := path.Base(strings.ReplaceAll(filename, "\\", "/"))
		if name == "" || name == "." || name == "/" {
			return ""
		}
		lower := strings.ToLower(name)
		for _, ext := range []string{
			".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst",
			".tgz", ".tbz2", ".txz",
			".tar", ".gz", ".zip", ".bz2", ".xz", ".zst",
		} {
			if strings.HasSuffix(lower, ext) {
				name = name[:len(name)-len(ext)]
				break
			}
		}
		name = strings.Trim(name, "._- ")
		return name
	}

	// volumeScanCandidates collects the app's live target volumes: declared
	// volumes for single-container apps (parseable offline), discovered mounts for
	// Compose apps (dynamically inspected on the agent, falling back to static
	// ComposeContent parsing when offline or stopped).
	func volumeScanCandidates(h *AppHandlers, app *Application) []volumeScanCandidate {
		if isComposeApp(app) {
			disc, _ := h.engine.DiscoverAppVolumes(app)
			if len(disc) == 0 {
				disc = staticComposeVolumes(app)
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
			src, dst, mode, ok := parseVolumeSpec(spec)
			if !ok || mode == "ro" {
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
	// scored by content shape and filename: an entry path matching the container path
	// is highest, archive filename stem or directory name matching the container
	// or host basename follows. When the application has only one writable volume,
	// it auto-pairs as single_candidate. All positive matches are returned.
	func matchScanVolumes(res *tarScanResult, cands []volumeScanCandidate, filename string) (match string, matched []volumeScanMatch) {
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
		stem := cleanArchiveStem(filename)

		for _, c := range cands {
			m := volumeScanMatch{
				VolumeSpec: c.Spec, ContainerPath: c.ContainerPath,
				HostPath: c.HostPath, Service: c.Service, VolumeType: c.VolumeType,
			}
			score := 0

			// 1. Exact or prefix match of container path inside tar (score: 30)
			if container := strings.TrimPrefix(c.ContainerPath, "/"); container != "" {
				if res.prefixes[container] {
					score, m.Reason = 30, "container_path"
				}
			}

			base := path.Base(c.ContainerPath)
			hostBase := path.Base(c.HostPath)
			if hostBase == "" || hostBase == "." || hostBase == "/" {
				if src, _, _, ok := parseVolumeSpec(c.Spec); ok {
					hostBase = path.Base(src)
				}
			}

			// 2. Archive filename stem matches container or host basename (score: 25 / 18)
			if stem != "" {
				stemLower := strings.ToLower(stem)
				baseLower := strings.ToLower(base)
				hostBaseLower := strings.ToLower(hostBase)
				if (baseLower != "" && baseLower != "/" && baseLower != "." && stemLower == baseLower) ||
					(hostBaseLower != "" && hostBaseLower != "/" && hostBaseLower != "." && stemLower == hostBaseLower) {
					if score < 25 {
						score, m.Reason = 25, "filename"
					}
				} else if (len(hostBaseLower) >= 3 && strings.Contains(stemLower, hostBaseLower)) ||
					(len(baseLower) >= 3 && strings.Contains(stemLower, baseLower)) {
					if score < 18 {
						score, m.Reason = 18, "filename"
					}
				}
			}

			// 3. Top-level directory inside tar matches container basename (score: 20)
			if base != "" && base != "/" && base != "." && res.topSet[base] {
				if score < 20 {
					score, m.Reason = 20, "basename"
				}
			}

			// 4. Top-level directory inside tar matches host basename (score: 15)
			if hostBase != "" && hostBase != "/" && hostBase != "." && res.topSet[hostBase] {
				if score < 15 {
					score, m.Reason = 15, "basename"
				}
			}

			if score > 0 {
				hits = append(hits, scored{m: m, score: score})
			}
		}

		if len(hits) == 0 {
			if len(cands) == 1 {
				return scanMatchUnique, []volumeScanMatch{{
					VolumeSpec:    cands[0].Spec,
					ContainerPath: cands[0].ContainerPath,
					HostPath:      cands[0].HostPath,
					Service:       cands[0].Service,
					VolumeType:    cands[0].VolumeType,
					Reason:        "single_candidate",
				}}
			}
			return scanMatchNone, matched
		}

		sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
		for _, h := range hits {
			matched = append(matched, h.m)
		}
		if len(matched) == 1 {
			return scanMatchUnique, matched
		}
		if hits[0].score > hits[1].score && hits[0].score >= 20 {
			return scanMatchUnique, []volumeScanMatch{hits[0].m}
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
			_, sDst, _, sOk := parseVolumeSpec(s)
			for i := range cands {
				if cands[i].Spec == s || cands[i].ContainerPath == s {
					found = &cands[i]
					break
				}
				if sOk && cands[i].ContainerPath != "" && cands[i].ContainerPath == sDst {
					found = &cands[i]
					break
				}
				if sOk {
					if _, cDst, _, cOk := parseVolumeSpec(cands[i].Spec); cOk && cDst == sDst {
						found = &cands[i]
						break
					}
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
