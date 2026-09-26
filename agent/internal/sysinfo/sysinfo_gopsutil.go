package sysinfo

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
)

// killProcessGopsutil terminates a single process. force selects SIGKILL
// (TerminateProcess on Windows) over SIGTERM. The agent's own process and PID 1
// are refused outright so a mis-click cannot take down the agent or init.
func killProcessGopsutil(pid int32, force bool) ([]byte, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("非法 PID: %d", pid)
	}
	if pid == 1 {
		return nil, fmt.Errorf("拒绝结束 PID 1 (init/系统进程)")
	}
	if int(pid) == os.Getpid() {
		return nil, fmt.Errorf("拒绝结束 Agent 自身进程 (PID %d)", pid)
	}
	p, err := process.NewProcess(pid)
	if err != nil {
		return nil, fmt.Errorf("进程 %d 不存在: %w", pid, err)
	}
	name, _ := p.Name()
	if force {
		err = p.Kill()
	} else {
		err = p.Terminate()
	}
	if err != nil {
		return nil, fmt.Errorf("结束进程 %d (%s) 失败: %w", pid, name, err)
	}
	return json.Marshal(map[string]any{
		"ok":    true,
		"pid":   pid,
		"name":  name,
		"force": force,
	})
}

func processListGopsutil() ([]byte, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}
	var list []Process
	for _, p := range procs {
		name, _ := p.Name()
		cpu, _ := p.CPUPercent()
		mem, _ := p.MemoryPercent()
		cmd, _ := p.Cmdline()
		user, _ := p.Username()
		list = append(list, Process{
			PID:     p.Pid,
			Name:    name,
			User:    user,
			CPU:     cpu,
			Mem:     float64(mem),
			Cmdline: cmd,
		})
		// Cap to 200 entries to avoid huge payloads.
		if len(list) >= 200 {
			break
		}
	}
	return json.Marshal(list)
}

func portListGopsutil() ([]byte, error) {
	conns, err := net.Connections("tcp")
	if err != nil {
		return nil, err
	}
	var list []Port
	for _, c := range conns {
		if c.Status == "LISTEN" {
			procName := ""
			if c.Pid > 0 {
				if p, err := process.NewProcess(c.Pid); err == nil {
					procName, _ = p.Name()
				}
			}
			list = append(list, Port{
				Proto:   "tcp",
				Address: c.Laddr.IP,
				Port:    c.Laddr.Port,
				State:   c.Status,
				PID:     c.Pid,
				Process: procName,
			})
		}
	}
	return json.Marshal(list)
}

func osInfoGopsutil() ([]byte, error) {
	info, err := host.Info()
	if err != nil {
		return nil, err
	}
	return json.Marshal(info)
}