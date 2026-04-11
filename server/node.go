package server

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type nodeInfoResponse struct {
	Hostname      string    `json:"hostname"`
	KernelVersion string    `json:"kernelVersion"`
	OSName        string    `json:"osName"`
	CPU           nodeCPU   `json:"cpu"`
	Memory        nodeMemory `json:"memory"`
	Disk          *nodeFS   `json:"disk,omitempty"`
	VMs           nodeVMs   `json:"vms"`
}

type nodeCPU struct {
	ModelName  string `json:"modelName"`
	TotalCores int    `json:"totalCores"`
}

type nodeMemory struct {
	TotalBytes     int64 `json:"totalBytes"`
	UsedBytes      int64 `json:"usedBytes"`
	FreeBytes      int64 `json:"freeBytes"`
	AvailableBytes int64 `json:"availableBytes"`
}

type nodeFS struct {
	TotalBytes int64 `json:"totalBytes"`
	UsedBytes  int64 `json:"usedBytes"`
	FreeBytes  int64 `json:"freeBytes"`
}

type nodeVMs struct {
	Total   int `json:"total"`
	Running int `json:"running"`
}

// GET /node
func (h *handlers) nodeInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	hostname, _, kernel := unameFields()

	info := nodeInfoResponse{
		Hostname:      hostname + ".local",
		KernelVersion: kernel,
		OSName:        osImage(),
		CPU:           cpuInfo(),
		Memory:        procMeminfo(),
		Disk:          diskInfo("/"),
		VMs:           vmCounts(ctx),
	}

	writeJSON(w, http.StatusOK, info)
}

// unameFields returns hostname, architecture, and kernel version via syscall.
func unameFields() (hostname, arch, kernel string) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		slog.Warn("uname failed", "err", err)
		return
	}
	return int8Slice(u.Nodename[:]), int8Slice(u.Machine[:]), int8Slice(u.Release[:])
}

// int8Slice converts a null-terminated int8 array (syscall.Utsname field) to a
// Go string.
func int8Slice(s []int8) string {
	b := make([]byte, 0, len(s))
	for _, v := range s {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return string(b)
}

// osImage reads PRETTY_NAME from /etc/os-release.
func osImage() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok && strings.TrimSpace(k) == "PRETTY_NAME" {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// cpuInfo reads model name and logical CPU count from /proc/cpuinfo.
func cpuInfo() nodeCPU {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		slog.Warn("open /proc/cpuinfo failed", "err", err)
		return nodeCPU{}
	}
	defer f.Close()

	var model string
	cores := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "model name":
			if model == "" {
				model = strings.TrimSpace(v)
			}
		case "processor":
			cores++
		}
	}
	return nodeCPU{ModelName: model, TotalCores: cores}
}

// procMeminfo reads MemTotal, MemFree, and MemAvailable from /proc/meminfo.
func procMeminfo() nodeMemory {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		slog.Warn("open /proc/meminfo failed", "err", err)
		return nodeMemory{}
	}
	defer f.Close()

	fields := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		// values are "NNN kB" — take the numeric token and convert to bytes
		parts := strings.Fields(strings.TrimSpace(v))
		if len(parts) == 0 {
			continue
		}
		n, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		fields[strings.TrimSpace(k)] = n * 1024
	}

	total := fields["MemTotal"]
	avail := fields["MemAvailable"]
	return nodeMemory{
		TotalBytes:     total,
		UsedBytes:      total - avail,
		FreeBytes:      fields["MemFree"],
		AvailableBytes: avail,
	}
}

// diskInfo returns filesystem usage for a single mount point, or nil if the
// path does not exist or cannot be stat'd.
func diskInfo(mountPoint string) *nodeFS {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountPoint, &st); err != nil {
		return nil
	}
	bs := int64(st.Bsize)
	total := int64(st.Blocks) * bs
	free := int64(st.Bfree) * bs
	return &nodeFS{
		TotalBytes: total,
		UsedBytes:  total - free,
		FreeBytes:  free,
	}
}

// vmCounts returns total and running VM counts from virsh list --all.
func vmCounts(ctx context.Context) nodeVMs {
	out, err := exec.CommandContext(ctx, "virsh", "list", "--all").Output()
	if err != nil {
		slog.Warn("virsh list failed", "err", err)
		return nodeVMs{}
	}
	vms := parseVirshList(string(out))
	counts := nodeVMs{Total: len(vms)}
	for _, vm := range vms {
		if vm.State == "running" {
			counts.Running++
		}
	}
	return counts
}
