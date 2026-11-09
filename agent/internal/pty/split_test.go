package pty

import (
	"reflect"
	"testing"
)

func TestSplitShellCommand(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"single word", "bash", []string{"bash"}, false},
		{"docker exec", "docker exec -it certimate /bin/sh", []string{"docker", "exec", "-it", "certimate", "/bin/sh"}, false},
		{"extra whitespace", "  docker   exec\t-it  certimate   ", []string{"docker", "exec", "-it", "certimate"}, false},
		{"single quotes", "sh -c 'echo hi'", []string{"sh", "-c", "echo hi"}, false},
		{"double quotes", `echo "hello world"`, []string{"echo", "hello world"}, false},
		{"escaped space", `echo a\ b`, []string{"echo", "a b"}, false},
		{"escaped quote in double quotes", `echo "a\"b"`, []string{"echo", `a"b`}, false},
		{"empty quotes", `echo ''`, []string{"echo", ""}, false},
		{"empty", "", nil, false},
		{"blank", "   ", nil, false},
		{"unterminated single", "echo 'abc", nil, true},
		{"unterminated double", `echo "abc`, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitShellCommand(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitShellCommand(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitShellCommand(%q) error: %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitShellCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
