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
