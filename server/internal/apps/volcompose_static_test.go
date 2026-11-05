package apps

import (
	"testing"

	"watchman/server/internal/rpc"
)

func TestStaticComposeVolumesParentRelativeAndFiltering(t *testing.T) {
	app := &Application{
		ID: "app1",
		ComposeContent: `
services:
  web:
    image: nginx
    volumes:
      - ../shared:/data
      - ./local:/local
      - myvol:/vol
      - a/../b:/bad
      - type: tmpfs
        target: /cache
      - type: volume
        target: /anon
      - type: bind
        source: ../bound
        target: /bound
      - source: ../untyped
        target: /untyped
`,
	}
	got := staticComposeVolumes(app)
	byContainer := map[string]DiscoveredVolume{}
	for _, v := range got {
		byContainer[v.ContainerPath] = v
	}
	if len(got) != 5 {
		t.Fatalf("got %d volumes %v, want 5", len(got), got)
	}
	if v := byContainer["/data"]; v.VolumeType != "bind" || v.HostPath != "/var/lib/watchman/apps/shared" {
		t.Errorf("/data = %+v, want bind /var/lib/watchman/apps/shared", v)
	}
	if v := byContainer["/local"]; v.VolumeType != "bind" || v.HostPath != "/var/lib/watchman/apps/app1/local" {
		t.Errorf("/local = %+v, want bind /var/lib/watchman/apps/app1/local", v)
	}
	if v := byContainer["/vol"]; v.VolumeType != "volume" || v.HostPath != "" {
		t.Errorf("/vol = %+v, want named volume with empty host path", v)
	}
	if v := byContainer["/bound"]; v.VolumeType != "bind" || v.HostPath != "/var/lib/watchman/apps/bound" {
		t.Errorf("/bound = %+v, want bind /var/lib/watchman/apps/bound", v)
	}
	if v := byContainer["/untyped"]; v.VolumeType != "bind" || v.HostPath != "/var/lib/watchman/apps/untyped" {
		t.Errorf("/untyped = %+v, want bind /var/lib/watchman/apps/untyped", v)
	}
	for _, dropped := range []string{"/bad", "/cache", "/anon"} {
		if _, ok := byContainer[dropped]; ok {
			t.Errorf("container path %s should have been skipped", dropped)
		}
	}
}

func TestDiscoverAppVolumesDynamicFailureIsError(t *testing.T) {
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	registerMigrateTestAgent(t, reg, "h1", "linux")
	e := NewEngine(reg, store, nil, nil)

	// Agent registered but never connected: dynamic discovery fails and
	// there is no compose file to fall back to — this must surface as an
	// error, not an empty volume list.
	app := &Application{ID: "app1", HostID: "h1", SourceType: "raw_compose"}
	if vols, err := e.DiscoverAppVolumes(app); err == nil {
		t.Fatalf("expected error, got volumes %v", vols)
	}

	// Same failed dynamic discovery, but a compose file exists: the static
	// fallback still wins and no error is reported.
	app2 := &Application{ID: "app2", HostID: "h1", SourceType: "raw_compose", ComposeContent: "services:\n  web:\n    image: nginx\n    volumes:\n      - ./data:/data\n"}
	vols, err := e.DiscoverAppVolumes(app2)
	if err != nil {
		t.Fatalf("static fallback should succeed, got error: %v", err)
	}
	if len(vols) != 1 || vols[0].ContainerPath != "/data" {
		t.Fatalf("static fallback volumes = %v, want one /data volume", vols)
	}
}

func TestMatchScanVolumesSingleWeakHitIsAmbiguous(t *testing.T) {
	cands := []volumeScanCandidate{
		{Spec: "/srv/app-data:/data", ContainerPath: "/data", HostPath: "/srv/app-data", VolumeType: "bind"},
		{Spec: "/srv/cache:/cache", ContainerPath: "/cache", HostPath: "/srv/cache", VolumeType: "bind"},
	}
	// Top-level dir matches only the first volume's host basename (15):
	// too weak to preselect a destructive restore target.
	res := &tarScanResult{prefixes: map[string]bool{}, topSet: map[string]bool{"app-data": true}}
	if match, matched := matchScanVolumes(res, cands, "backup.tar.gz"); match != scanMatchAmbiguous || len(matched) != 1 {
		t.Fatalf("weak single hit: match=%q matched=%v, want ambiguous with 1 candidate", match, matched)
	}
	// Top-level dir matches the container basename (20): clears the bar.
	res2 := &tarScanResult{prefixes: map[string]bool{}, topSet: map[string]bool{"data": true}}
	if match, matched := matchScanVolumes(res2, cands, "backup.tar.gz"); match != scanMatchUnique || len(matched) != 1 {
		t.Fatalf("strong single hit: match=%q matched=%v, want unique with 1 candidate", match, matched)
	}
}

func TestStaticComposeVolumesContainmentAndEdgeCases(t *testing.T) {
	app := &Application{
		ID: "app1",
		ComposeContent: `
services:
  web:
    image: nginx
    volumes:
      - .\data:/winrel
      - .:/appdir
      - ../..:/escape1
      - ../../../../etc:/escape2
      - ../shared:/shared
      - /etc/localtime:/etc/localtime:ro
      - /var/run/docker.sock:/var/run/docker.sock
      - type: tmpfs
        target: /cache
      - type: volume
        target: /anon
      - type: bind
        source: ../../../../etc
        target: /longescape
      - type: volume
        source: namedvol
        target: /named
`,
	}
	got := staticComposeVolumes(app)
	byContainer := map[string]DiscoveredVolume{}
	for _, v := range got {
		if v.VolumeSpec == "" || v.VolumeSpec[0] == ':' {
			t.Errorf("malformed VolumeSpec %q for %s", v.VolumeSpec, v.ContainerPath)
		}
		byContainer[v.ContainerPath] = v
	}
	// Sources whose resolved host path escapes the apps root must never
	// become a clean-restore (rm -rf / untar) target.
	for _, cp := range []string{"/escape1", "/escape2", "/longescape"} {
		if v, ok := byContainer[cp]; ok {
			t.Errorf("%s must be dropped (escapes apps root), got %+v", cp, v)
		}
	}
	// Legitimate parent-relative and current-dir binds stay.
	if v := byContainer["/shared"]; v.VolumeType != "bind" || v.HostPath != "/var/lib/watchman/apps/shared" || v.VolumeSpec != "../shared:/shared" {
		t.Errorf("/shared = %+v, want bind ../shared:/shared -> /var/lib/watchman/apps/shared", v)
	}
	if v := byContainer["/appdir"]; v.VolumeType != "bind" || v.HostPath != "/var/lib/watchman/apps/app1" {
		t.Errorf("/appdir = %+v, want bind /var/lib/watchman/apps/app1", v)
	}
	if v := byContainer["/winrel"]; v.VolumeType != "bind" || v.HostPath != "/var/lib/watchman/apps/app1/data" {
		t.Errorf("/winrel = %+v, want bind /var/lib/watchman/apps/app1/data", v)
	}
	// System mounts, tmpfs and sourceless anonymous volumes are filtered.
	for _, cp := range []string{"/etc/localtime", "/var/run/docker.sock", "/cache", "/anon"} {
		if v, ok := byContainer[cp]; ok {
			t.Errorf("%s must be filtered, got %+v", cp, v)
		}
	}
	// A named volume stays, with a well-formed spec and empty host path.
	if v := byContainer["/named"]; v.VolumeType != "volume" || v.HostPath != "" || v.VolumeSpec != "namedvol:/named" {
		t.Errorf("/named = %+v, want named volume namedvol:/named", v)
	}
}

// TestVolumeScanCandidatesSurfacesDiscoveryError covers the same dynamic
// discovery failure as TestDiscoverAppVolumesDynamicFailureIsError, but keeps
// the engine registry nil so discovery fails fast offline instead of waiting
// on the 60s execOnAgent timeout of a registered-but-unconnected agent.
func TestVolumeScanCandidatesSurfacesDiscoveryError(t *testing.T) {
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	e := NewEngine(nil, store, nil, nil)
	h := NewAppHandlers(nil, store, e)

	// No ComposeContent: discovery fails and static parsing finds nothing —
	// the error must not be swallowed into "no candidates".
	app := &Application{ID: "app1", HostID: "h1", SourceType: "raw_compose"}
	cands, err := volumeScanCandidates(h, app)
	if err == nil {
		t.Fatalf("expected discovery error, got candidates %v", cands)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates must be empty on error, got %v", cands)
	}

	// Static ComposeContent still wins over a failed discovery (offline
	// fallback semantics stay intact).
	app2 := &Application{ID: "app2", HostID: "h1", SourceType: "raw_compose",
		ComposeContent: "services:\n  web:\n    image: nginx\n    volumes:\n      - ./data:/data\n"}
	cands, err = volumeScanCandidates(h, app2)
	if err != nil {
		t.Fatalf("static fallback should succeed, got error: %v", err)
	}
	if len(cands) != 1 || cands[0].ContainerPath != "/data" {
		t.Fatalf("static fallback candidates = %v, want one /data volume", cands)
	}
}
