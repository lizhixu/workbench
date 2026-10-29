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
	"time"
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
