package panelsec

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// portOwner identifies one process listening on a TCP port.
type portOwner struct {
	PID  int
	Name string
}

func (o portOwner) String() string {
	name := strings.TrimSpace(o.Name)
	if name == "" {
		name = "未知进程"
	}
	return fmt.Sprintf("%s (pid %d)", name, o.PID)
}

// detectPortOwner reports the processes currently listening on port. It is
// best effort and bounded to a few seconds: the UI only needs "who holds :80"
// as a hint for the bind failure, so every failure mode simply yields no
// owners instead of an error.
func detectPortOwner(port int) []portOwner {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if runtime.GOOS == "windows" {
		return detectPortOwnerWindows(ctx, port)
	}
	if owners, ok := detectPortOwnerProc(port); ok {
		return owners
	}
	return detectPortOwnerLsof(ctx, port)
}

// detectPortOwnerProc maps the listening socket's inode (from /proc/net/tcp)
// back to its process by scanning /proc/*/fd. It needs no external binary and
// works on any Linux with /proc mounted; non-root callers only see their own
// processes, which matches what the kernel lets them inspect anyway.
func detectPortOwnerProc(port int) ([]portOwner, bool) {
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		return nil, false
	}
	inodes := map[string]bool{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		collectListeningInodes(string(data), port, inodes)
	}
	if len(inodes) == 0 {
		return nil, true
	}
	return ownersBySocketInode(inodes), true
}

// collectListeningInodes parses /proc/net/tcp content: hex local port in
// column 2, state 0A (LISTEN) in column 4, socket inode in column 10.
func collectListeningInodes(data string, port int, inodes map[string]bool) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		idx := strings.LastIndex(fields[1], ":")
		if idx < 0 {
			continue
		}
		n, err := strconv.ParseInt(fields[1][idx+1:], 16, 32)
		if err != nil || int(n) != port {
			continue
		}
		if inode := fields[9]; inode != "" && inode != "0" {
			inodes[inode] = true
		}
	}
}

func ownersBySocketInode(inodes map[string]bool) []portOwner {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var owners []portOwner
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || !proc.IsDir() {
			continue
		}
		fdDir := filepath.Join("/proc", proc.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if !inodes[inode] {
				continue
			}
			name := ""
			if b, err := os.ReadFile(filepath.Join("/proc", proc.Name(), "comm")); err == nil {
				name = strings.TrimSpace(string(b))
			}
			owners = append(owners, portOwner{PID: pid, Name: name})
			break
		}
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].PID < owners[j].PID })
	return owners
}

func detectPortOwnerWindows(ctx context.Context, port int) []portOwner {
	out, err := exec.CommandContext(ctx, "netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return nil
	}
	pids := netstatListenPIDs(string(out), port)
	if len(pids) == 0 {
		return nil
	}
	names := windowsTaskNames(ctx)
	owners := make([]portOwner, 0, len(pids))
	for _, pid := range pids {
		owners = append(owners, portOwner{PID: pid, Name: names[pid]})
	}
	return owners
}

// netstatListenPIDs extracts the owning PIDs from `netstat -ano -p tcp`
// output: protocol, local address, foreign address, state, PID.
func netstatListenPIDs(data string, port int) []int {
	suffix := ":" + strconv.Itoa(port)
	seen := map[int]bool{}
	var pids []int
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[0], "TCP") {
			continue
		}
		if !strings.EqualFold(fields[3], "LISTENING") || !strings.HasSuffix(fields[1], suffix) {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

func windowsTaskNames(ctx context.Context) map[int]string {
	out, err := exec.CommandContext(ctx, "tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return nil
	}
	return parseTasklistCSV(string(out))
}

// parseTasklistCSV maps PID to image name from `tasklist /FO CSV /NH` rows
// (`"name","pid",...`).
func parseTasklistCSV(data string) map[int]string {
	names := map[int]string{}
	r := csv.NewReader(strings.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	for {
		rec, err := r.Read()
		if err != nil {
			break
		}
		if len(rec) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(rec[1]))
		if err != nil {
			continue
		}
		names[pid] = strings.TrimSpace(rec[0])
	}
	return names
}

func detectPortOwnerLsof(ctx context.Context, port int) []portOwner {
	out, err := exec.CommandContext(ctx, "lsof", "-nP",
		fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-F", "pc").Output()
	if err != nil {
		return nil
	}
	return parseLsofListeners(string(out))
}

// parseLsofListeners parses `lsof -F pc` output ("p<pid>" then "c<command>").
func parseLsofListeners(data string) []portOwner {
	seen := map[int]bool{}
	var owners []portOwner
	var cur *portOwner
	flush := func() {
		if cur == nil {
			return
		}
		if !seen[cur.PID] {
			seen[cur.PID] = true
			owners = append(owners, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			flush()
			pid, err := strconv.Atoi(line[1:])
			if err != nil {
				continue
			}
			cur = &portOwner{PID: pid}
		case 'c':
			if cur != nil {
				cur.Name = line[1:]
			}
		}
	}
	flush()
	sort.Slice(owners, func(i, j int) bool { return owners[i].PID < owners[j].PID })
	return owners
}

// dockerProxyOwners reports whether any owner is Docker's port-forwarding
// shim (docker-proxy / Docker Desktop backend). Those processes only exist to
// forward a published container port, so the container itself is the
// actionable identity for the user.
func dockerProxyOwners(owners []portOwner) bool {
	for _, o := range owners {
		if strings.Contains(strings.ToLower(o.Name), "docker") {
			return true
		}
	}
	return false
}

// dockerContainersPublishing names the containers publishing hostPort. Best
// effort: a missing docker CLI (or timeout) yields no names and the caller
// falls back to the raw process list.
func dockerContainersPublishing(port int) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}\t{{.Ports}}").Output()
	if err != nil {
		return nil
	}
	return parseDockerPSPublishing(string(out), port)
}

// parseDockerPSPublishing extracts container names whose published ports
// include hostPort, from `docker ps --format '{{.Names}}\t{{.Ports}}'` rows
// (`name<TAB>0.0.0.0:443->443/tcp, [::]:443->443/tcp`).
func parseDockerPSPublishing(data string, hostPort int) []string {
	needle := fmt.Sprintf(":%d->", hostPort)
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(data, "\n") {
		name, ports, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		name = strings.TrimSpace(strings.TrimPrefix(name, "/"))
		if name == "" || seen[name] || !strings.Contains(ports, needle) {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// containerAppResolver maps a panel-managed container name (watchman-app-* /
// watchman-fwd-*) to its application display name. main wires it to the app
// store; nil (tests, watchmanctl) keeps the raw container name.
var (
	containerAppMu       sync.RWMutex
	containerAppResolver func(container string) string
)

// SetContainerAppResolver installs the container-to-application name lookup.
func SetContainerAppResolver(fn func(container string) string) {
	containerAppMu.Lock()
	defer containerAppMu.Unlock()
	containerAppResolver = fn
}

func containerAppName(container string) string {
	containerAppMu.RLock()
	fn := containerAppResolver
	containerAppMu.RUnlock()
	if fn == nil {
		return ""
	}
	return fn(container)
}

// portErrorWithOwnerDetail prefers the container name over docker-proxy PIDs
// (two proxies per port — IPv4 and IPv6 — are pure noise to the operator).
func portErrorWithOwnerDetail(base string, port int, owners []portOwner) string {
	if len(owners) == 0 {
		return base
	}
	if dockerProxyOwners(owners) {
		if names := dockerContainersPublishing(port); len(names) > 0 {
			return portErrorWithContainers(base, names)
		}
	}
	return portErrorWithOwners(base, owners)
}

// portErrorWithContainers names the containers publishing the port. Panel's
// own app containers are upgraded to their application name so the operator
// knows which app to move off the port.
func portErrorWithContainers(base string, names []string) string {
	const maxNames = 3
	shown := names
	if len(shown) > maxNames {
		shown = shown[:maxNames]
	}
	labels := make([]string, 0, len(shown))
	for _, name := range shown {
		if appName := containerAppName(name); appName != "" {
			labels = append(labels, fmt.Sprintf("应用「%s」（容器 %s）", appName, name))
			continue
		}
		labels = append(labels, "容器 "+name)
	}
	holders := strings.Join(labels, "、")
	if len(names) > maxNames {
		holders += fmt.Sprintf(" 等 %d 个容器", len(names))
	}
	// Keep a space before 占用 when the name ends in ASCII (容器 traefik 占用),
	// omit it after CJK punctuation (应用「x」（容器 y）占用).
	sep := ""
	if last, _ := utf8.DecodeLastRuneInString(holders); last < 0x2E80 {
		sep = " "
	}
	return fmt.Sprintf("端口被%s%s占用（释放后 30 秒内自动恢复）", holders, sep)
}

// portErrorWithOwners turns the generic "port in use" message into one that
// names the holding processes; the generic message is kept when detection
// found nobody (permission-restricted socket, missing /proc, minimal image).
func portErrorWithOwners(base string, owners []portOwner) string {
	if len(owners) == 0 {
		return base
	}
	const maxOwners = 3
	parts := make([]string, 0, maxOwners)
	for i, o := range owners {
		if i == maxOwners {
			break
		}
		parts = append(parts, o.String())
	}
	holders := strings.Join(parts, "、")
	if len(owners) > maxOwners {
		holders += fmt.Sprintf(" 等 %d 个进程", len(owners))
	}
	return fmt.Sprintf("端口被 %s 占用（释放后 30 秒内自动恢复）", holders)
}
