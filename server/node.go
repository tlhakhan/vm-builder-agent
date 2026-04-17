package server

import (
	"bufio"
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type nodeInfoResponse struct {
	Hostname      string          `json:"hostname"`
	KernelVersion string          `json:"kernel_version"`
	OSName        string          `json:"os_name"`
	CPU           nodeCPU         `json:"cpu"`
	Memory        nodeMemory      `json:"memory"`
	Disk          *nodeFS         `json:"disk,omitempty"`
	VMs           nodeVMs         `json:"vms"`
	PCIDevices    []nodePCIDevice `json:"pci_devices"`
}

type nodeCPU struct {
	ModelName  string `json:"model_name"`
	TotalCores int    `json:"total_cores"`
}

type nodeMemory struct {
	TotalBytes     int64 `json:"total_bytes"`
	UsedBytes      int64 `json:"used_bytes"`
	FreeBytes      int64 `json:"free_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

type nodeFS struct {
	TotalBytes int64 `json:"total_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
}

type nodeVMs struct {
	Total   int `json:"total"`
	Running int `json:"running"`
}

type nodePCIDevice struct {
	Address    string `json:"address"`
	Class      string `json:"class"`
	ClassID    string `json:"class_id"`
	Vendor     string `json:"vendor"`
	VendorID   string `json:"vendor_id"`
	Name       string `json:"name"`
	DeviceID   string `json:"device_id"`
	SubVendor  string `json:"sub_vendor,omitempty"`
	SubDevice  string `json:"sub_device,omitempty"`
	Revision   string `json:"revision,omitempty"`
	IOMMUGroup int    `json:"iommu_group"`
	Available  bool   `json:"available"`
	AttachedTo string `json:"attached_to,omitempty"`
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
		PCIDevices:    pciPassthroughDevices(ctx),
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

const vfioPCIDriverPath = "/sys/bus/pci/drivers/vfio-pci"

// pciPassthroughDevices returns PCI devices currently bound to the vfio-pci driver,
// annotated with whether they are attached to a running VM.
func pciPassthroughDevices(ctx context.Context) []nodePCIDevice {
	entries, err := os.ReadDir(vfioPCIDriverPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("reading vfio-pci driver dir failed", "err", err)
		}
		return nil
	}

	attached := attachedVFIODevices(ctx)

	var devices []nodePCIDevice
	for _, e := range entries {
		// BDF addresses look like "0000:01:00.0" — skip control files.
		name := e.Name()
		if !strings.Contains(name, ":") {
			continue
		}
		if dev, ok := readPCIDevice(ctx, name); ok {
			if vm, inUse := attached[name]; inUse {
				dev.AttachedTo = vm
			} else {
				dev.Available = true
			}
			devices = append(devices, dev)
		}
	}
	return devices
}

func readPCIDevice(ctx context.Context, addr string) (nodePCIDevice, bool) {
	sysfsBase := filepath.Join("/sys/bus/pci/devices", addr)

	readAttr := func(attr string) string {
		b, err := os.ReadFile(filepath.Join(sysfsBase, attr))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}

	vendorHex := strings.TrimPrefix(readAttr("vendor"), "0x")
	deviceHex := strings.TrimPrefix(readAttr("device"), "0x")
	classHex := strings.TrimPrefix(readAttr("class"), "0x")
	revHex := strings.TrimPrefix(readAttr("revision"), "0x")

	// Class ID is the top two bytes of the 3-byte class code (e.g. "030000" → "0300").
	classID := classHex
	if len(classHex) >= 4 {
		classID = classHex[:4]
	}

	dev := nodePCIDevice{
		Address:  addr,
		ClassID:  classID,
		VendorID: vendorHex,
		DeviceID: deviceHex,
		Revision: revHex,
	}

	// IOMMU group number from the symlink target's last path component.
	if link, err := os.Readlink(filepath.Join(sysfsBase, "iommu_group")); err == nil {
		if n, err := strconv.Atoi(filepath.Base(link)); err == nil {
			dev.IOMMUGroup = n
		}
	}

	// Human-readable names from lspci.
	out, err := exec.CommandContext(ctx, "lspci", "-Dvmm", "-s", addr).Output()
	if err != nil {
		slog.Warn("lspci failed", "addr", addr, "err", err)
		return dev, true
	}
	parseLspciVmm(string(out), &dev)
	return dev, true
}

// parseLspciVmm fills human-readable fields from `lspci -Dvmm` output.
func parseLspciVmm(out string, dev *nodePCIDevice) {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Class":
			dev.Class = v
		case "Vendor":
			dev.Vendor = v
		case "Device":
			dev.Name = v
		case "SVendor":
			dev.SubVendor = v
		case "SDevice":
			dev.SubDevice = v
		}
	}
}

// virshDomain is a minimal representation of a libvirt domain XML used to
// extract PCI hostdev source addresses.
type virshDomain struct {
	Devices struct {
		Hostdevs []struct {
			Mode string `xml:"mode,attr"`
			Type string `xml:"type,attr"`
			Source struct {
				Address struct {
					Domain   string `xml:"domain,attr"`
					Bus      string `xml:"bus,attr"`
					Slot     string `xml:"slot,attr"`
					Function string `xml:"function,attr"`
				} `xml:"address"`
			} `xml:"source"`
		} `xml:"hostdev"`
	} `xml:"devices"`
}

// attachedVFIODevices returns a map of PCI BDF address → VM name for every
// PCI device currently passed through to a running VM.
func attachedVFIODevices(ctx context.Context) map[string]string {
	out, err := exec.CommandContext(ctx, "virsh", "list", "--state-running", "--name").Output()
	if err != nil {
		slog.Warn("virsh list --state-running failed", "err", err)
		return nil
	}

	attached := map[string]string{}
	for _, vmName := range strings.Fields(string(out)) {
		xmlOut, err := exec.CommandContext(ctx, "virsh", "dumpxml", vmName).Output()
		if err != nil {
			slog.Warn("virsh dumpxml failed", "vm", vmName, "err", err)
			continue
		}
		var domain virshDomain
		if err := xml.Unmarshal(xmlOut, &domain); err != nil {
			slog.Warn("parsing domain XML failed", "vm", vmName, "err", err)
			continue
		}
		for _, hd := range domain.Devices.Hostdevs {
			if hd.Type != "pci" || hd.Mode != "subsystem" {
				continue
			}
			bdf := pciAddrToBDF(hd.Source.Address.Domain, hd.Source.Address.Bus,
				hd.Source.Address.Slot, hd.Source.Address.Function)
			if bdf != "" {
				attached[bdf] = vmName
			}
		}
	}
	return attached
}

// pciAddrToBDF converts libvirt hex address components (e.g. "0x0000", "0x01",
// "0x00", "0x0") to a canonical BDF string ("0000:01:00.0").
func pciAddrToBDF(domain, bus, slot, function string) string {
	parse := func(s string) (uint64, error) {
		s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
		return strconv.ParseUint(s, 16, 64)
	}
	d, err1 := parse(domain)
	b, err2 := parse(bus)
	sl, err3 := parse(slot)
	f, err4 := parse(function)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return ""
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", d, b, sl, f)
}
