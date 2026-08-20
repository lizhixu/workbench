package sysinfo

import (
	"encoding/json"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
)

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