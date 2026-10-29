package panelsec

import (
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCollectListeningInodes(t *testing.T) {
	const procNet = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 111 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 222 1 0000000000000000 100 0 0 10 0
   2: 00000000:0050 00000000:0000 01 00000000:00000000 00:00000000 00000000     0        0 333 1 0000000000000000 100 0 0 10 0
`
	inodes := map[string]bool{}
	collectListeningInodes(procNet, 80, inodes)
	if len(inodes) != 1 || !inodes["111"] {
		t.Fatalf("port 80 inodes = %v, want only 111 (listening rows, not established)", inodes)
	}
	inodes = map[string]bool{}
	collectListeningInodes(procNet, 8080, inodes)
	if len(inodes) != 1 || !inodes["222"] {
		t.Fatalf("port 8080 inodes = %v, want only 222", inodes)
	}
}

func TestNetstatListenPIDs(t *testing.T) {
	const out = "\n活动连接\n\n  协议  本地地址          外部地址        状态           PID\n" +
		"  TCP    0.0.0.0:80             0.0.0.0:0              LISTENING       4\n" +
		"  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       944\n" +
		"  TCP    [::]:80                [::]:0                 LISTENING       4\n" +
		"  TCP    0.0.0.0:18080          0.0.0.0:0              LISTENING       9999\n" +
		"  TCP    127.0.0.1:50000        127.0.0.1:80           ESTABLISHED     7777\n"
	got := netstatListenPIDs(out, 80)
	if len(got) != 1 || got[0] != 4 {
		t.Fatalf("netstatListenPIDs(:80) = %v, want [4] (IPv6 duplicate deduped, no client row)", got)
	}
}

func TestParseTasklistCSV(t *testing.T) {
	const out = "\"System\",\"4\",\"Services\",\"0\",\"144 K\"\n" +
		"\"nginx.exe\",\"1234\",\"Console\",\"1\",\"12,345 K\"\n"
	names := parseTasklistCSV(out)
	if names[4] != "System" || names[1234] != "nginx.exe" {
		t.Fatalf("parseTasklistCSV = %v", names)
	}
}

func TestParseLsofListeners(t *testing.T) {
	const out = "p1234\ncnginx\np1234\ncnginx\np5678\ncdocker-proxy\n"
	owners := parseLsofListeners(out)
	if len(owners) != 2 || owners[0].PID != 1234 || owners[0].Name != "nginx" ||
		owners[1].PID != 5678 || owners[1].Name != "docker-proxy" {
		t.Fatalf("parseLsofListeners = %+v", owners)
	}
}

func TestPortErrorWithOwners(t *testing.T) {
	base := "端口被其他服务占用（释放后 30 秒内自动恢复）"
	if got := portErrorWithOwners(base, nil); got != base {
		t.Fatalf("no owners = %q, want base message", got)
	}
	got := portErrorWithOwners(base, []portOwner{{PID: 1234, Name: "nginx"}})
	want := "端口被 nginx (pid 1234) 占用（释放后 30 秒内自动恢复）"
	if got != want {
		t.Fatalf("one owner = %q, want %q", got, want)
	}
	got = portErrorWithOwners(base, []portOwner{
		{PID: 1, Name: "a"}, {PID: 2, Name: "b"}, {PID: 3, Name: "c"}, {PID: 4, Name: ""},
	})
	if !strings.Contains(got, "a (pid 1)") || !strings.Contains(got, "等 4 个进程") {
		t.Fatalf("many owners = %q, want first three plus a count", got)
	}
}

func TestParseDockerPSPublishing(t *testing.T) {
	const out = "watchman-app-abcd1234\t0.0.0.0:443->443/tcp, [::]:443->443/tcp\n" +
		"other-app\t0.0.0.0:1443->1443/tcp\n" +
		"multi\t127.0.0.1:80->80/tcp, 0.0.0.0:443->8443/tcp\n" +
		"no-tab-line\n"
	got := parseDockerPSPublishing(out, 443)
	want := []string{"multi", "watchman-app-abcd1234"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDockerPSPublishing(:443) = %v, want %v (no :1443 false positive)", got, want)
	}
	if got := parseDockerPSPublishing(out, 8080); len(got) != 0 {
		t.Fatalf("parseDockerPSPublishing(:8080) = %v, want empty", got)
	}
}

func TestDockerProxyOwners(t *testing.T) {
	if dockerProxyOwners([]portOwner{{PID: 1, Name: "nginx"}}) {
		t.Fatal("nginx must not be treated as a docker proxy")
	}
	if !dockerProxyOwners([]portOwner{{PID: 1, Name: "docker-proxy"}}) {
		t.Fatal("docker-proxy must be detected")
	}
	if !dockerProxyOwners([]portOwner{{PID: 1, Name: "com.docker.backend"}}) {
		t.Fatal("Docker Desktop backend must be detected")
	}
}

func TestPortErrorWithOwnerDetailFallsBackForNonDocker(t *testing.T) {
	base := "端口被其他服务占用（释放后 30 秒内自动恢复）"
	if got := portErrorWithOwnerDetail(base, 443, nil); got != base {
		t.Fatalf("no owners = %q, want base message", got)
	}
	got := portErrorWithOwnerDetail(base, 443, []portOwner{{PID: 1234, Name: "nginx"}})
	want := "端口被 nginx (pid 1234) 占用（释放后 30 秒内自动恢复）"
	if got != want {
		t.Fatalf("non-docker owner = %q, want %q", got, want)
	}
}

func TestPortErrorWithContainers(t *testing.T) {
	base := "端口被其他服务占用（释放后 30 秒内自动恢复）"
	SetContainerAppResolver(func(container string) string {
		if container == "watchman-app-app_shop" {
			return "商城"
		}
		return ""
	})
	defer SetContainerAppResolver(nil)

	got := portErrorWithContainers(base, []string{"watchman-app-app_shop", "traefik"})
	want := "端口被应用「商城」（容器 watchman-app-app_shop）、容器 traefik 占用（释放后 30 秒内自动恢复）"
	if got != want {
		t.Fatalf("mixed owners = %q, want %q", got, want)
	}

	got = portErrorWithContainers(base, []string{"a", "b", "c", "d"})
	if !strings.Contains(got, "容器 a、容器 b、容器 c 等 4 个容器占用") {
		t.Fatalf("many containers = %q, want first three plus a count", got)
	}
}

func TestStandardPortErrorMessages(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("listen tcp :80: bind: address already in use"), "端口被其他服务占用（释放后 30 秒内自动恢复）"},
		{errors.New("listen tcp :80: bind: Only one usage of each socket address is normally permitted."), "端口被其他服务占用（释放后 30 秒内自动恢复）"},
		{errors.New("listen tcp :80: bind: permission denied"), "无权限监听（需要 root 或 CAP_NET_BIND_SERVICE 权限）"},
		{errors.New("listen tcp :80: invalid argument"), "监听失败：listen tcp :80: invalid argument"},
	}
	for _, tc := range cases {
		if got := standardPortError(tc.err); got != tc.want {
			t.Errorf("standardPortError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestDetectPortOwnerSelf binds an ephemeral port and expects the detection to
// name this (test) process. Environments where the OS blocks process
// introspection (containers without /proc, unprivileged sandboxes) skip.
func TestDetectPortOwnerSelf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	owners := detectPortOwner(port)
	if len(owners) == 0 {
		t.Skip("当前环境无法探测端口占用进程（缺少 /proc、lsof 或系统命令）")
	}
	pid := os.Getpid()
	for _, o := range owners {
		if o.PID == pid {
			return
		}
	}
	t.Fatalf("detectPortOwner(:%d) = %+v, want own pid %d", port, owners, pid)
}
