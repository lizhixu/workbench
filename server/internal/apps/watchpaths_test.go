package apps

import (
	"reflect"
	"testing"
)

func TestNormalizeWatchPaths(t *testing.T) {
	cases := []struct {
		in      []string
		want    []string
		wantErr bool
	}{
		{nil, nil, false},
		{[]string{"apps/web", "docker/"}, []string{"apps/web", "docker"}, false},
		{[]string{" /apps/web/ ", "apps//web", "apps/web"}, []string{"apps/web"}, false},
		{[]string{"", "  ", "apps/web"}, []string{"apps/web"}, false},
		{[]string{"/"}, nil, false},
		{[]string{"docker-compose.yml"}, []string{"docker-compose.yml"}, false},
		{[]string{"apps/../web"}, nil, true},
		{[]string{"../secret"}, nil, true},
		{[]string{`apps\web`}, nil, true},
		{[]string{"apps/./web"}, []string{"apps/web"}, false},
	}
	for _, tc := range cases {
		got, err := NormalizeWatchPaths(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeWatchPaths(%q) should fail", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeWatchPaths(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("NormalizeWatchPaths(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWatchPathsMatch(t *testing.T) {
	cases := []struct {
		watch, changed []string
		want           bool
	}{
		{nil, []string{"README.md"}, true}, // empty watch = watch everything
		{[]string{}, []string{"README.md"}, true},
		{[]string{"apps/web"}, []string{"apps/web/src/index.ts"}, true},
		{[]string{"apps/web"}, []string{"apps/web"}, true},
		{[]string{"apps/web"}, []string{"apps/website/x"}, false}, // boundary, not a string prefix
		{[]string{"apps/web"}, []string{"apps/web2/x"}, false},
		{[]string{"docker-compose.yml"}, []string{"docker-compose.yml"}, true},
		{[]string{"docker-compose.yml"}, []string{"docker-compose.yml.bak"}, false},
		{[]string{"apps/web", "docker/"}, []string{"README.md", "docker/Dockerfile"}, true},
		{[]string{"apps/web"}, []string{"README.md"}, false},
		{[]string{"apps/web"}, nil, false},
		{[]string{"Apps/Web"}, []string{"apps/web/x"}, false}, // case-sensitive
		{[]string{"/apps/web/"}, []string{"/apps/web/x"}, true},
		{[]string{"apps/web"}, []string{"apps/web/./x"}, true},
	}
	for _, tc := range cases {
		if got := WatchPathsMatch(tc.watch, tc.changed); got != tc.want {
			t.Errorf("WatchPathsMatch(%q, %q) = %v, want %v", tc.watch, tc.changed, got, tc.want)
		}
	}
}
