package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tlhakhan/vm-builder-agent/runner"
)

type handlers struct {
	runner *runner.Runner
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "err", err)
	}
}

func writeErrorJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// vmActionResponse is returned by create and delete endpoints.
type vmActionResponse struct {
	Name   string `json:"name"`
	Output string `json:"output,omitempty"`
}

// POST /vm/create
func (h *handlers) createVM(w http.ResponseWriter, r *http.Request) {
	var params runner.VMParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if params.Name == "" {
		writeErrorJSON(w, http.StatusBadRequest, "name is required")
		return
	}

	slog.Info("creating VM", "vm_name", params.Name)
	output, err := h.runner.Create(r.Context(), params)
	if err != nil {
		var exists runner.ErrVMExists
		var locked runner.ErrVMLocked
		if errors.As(err, &exists) || errors.As(err, &locked) {
			writeErrorJSON(w, http.StatusConflict, err.Error())
		} else {
			writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	slog.Info("VM created", "vm_name", params.Name)
	writeJSON(w, http.StatusOK, vmActionResponse{Name: params.Name, Output: output})
}

// DELETE /vm/{name}
func (h *handlers) deleteVM(w http.ResponseWriter, r *http.Request) {
	vmName := r.PathValue("name")
	if vmName == "" {
		writeErrorJSON(w, http.StatusBadRequest, "vm name is required")
		return
	}

	slog.Info("deleting VM", "vm_name", vmName)
	output, err := h.runner.Destroy(r.Context(), vmName)
	if err != nil {
		var locked runner.ErrVMLocked
		if errors.As(err, &locked) {
			writeErrorJSON(w, http.StatusConflict, err.Error())
		} else {
			writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	slog.Info("VM deleted", "vm_name", vmName)
	writeJSON(w, http.StatusOK, vmActionResponse{Name: vmName, Output: output})
}

// virshVM is one row from virsh list --all.
type virshVM struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// GET /vm — runs virsh list --all and returns the parsed output as JSON.
func (h *handlers) listVMs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "virsh", "list", "--all").Output()
	if err != nil {
		slog.Error("virsh list failed", "err", err)
		writeErrorJSON(w, http.StatusInternalServerError, "virsh list failed: "+err.Error())
		return
	}

	vms := parseVirshList(string(out))
	if vms == nil {
		vms = []virshVM{}
	}
	writeJSON(w, http.StatusOK, vms)
}

// parseVirshList converts the text table output of virsh list --all into a
// slice of virshVM structs.
//
// Example virsh output (lines 0-1 are header, line 2+ are data):
//
//	Id   Name        State
//	---  ----------  --------
//	1    myvm        running
//	-    stoppedvm   shut off
func parseVirshList(output string) []virshVM {
	var vms []virshVM
	lines := strings.Split(output, "\n")
	// Skip the two header lines; stop on empty trailing line.
	for _, line := range lines[2:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// Fields: id, name, state... (state can be two words: "shut off")
		vm := virshVM{
			ID:    fields[0],
			Name:  fields[1],
			State: strings.Join(fields[2:], " "),
		}
		vms = append(vms, vm)
	}
	return vms
}

// vmInfo holds the parsed output of virsh dominfo combined with the
// non-sensitive creation parameters from the VM's terraform workspace.
type vmInfo struct {
	// from virsh dominfo
	Name       string `json:"name"`
	State      string `json:"state"`
	CPUs       string `json:"cpus"`
	MaxMemory  string `json:"max_memory"`
	UsedMemory string `json:"used_memory"`
	Persistent string `json:"persistent"`
	Autostart  string `json:"autostart"`
	// from terraform.tfvars (omitted when workspace is absent)
	CreationParams *publicVMParamsResponse `json:"creation_params,omitempty"`
}

type publicVMParamsResponse struct {
	Name                 string `json:"name"`
	CPU                  int    `json:"cpu"`
	MemoryGiB            int    `json:"memory_gib"`
	DisksGiB             []int  `json:"disks_gib"`
	CloudImageURL        string `json:"cloud_image_url"`
	ConsoleUser          string `json:"console_user"`
	AutomationUser       string `json:"automation_user"`
	AutomationUserPubkey string `json:"automation_user_pubkey"`
	PCIDevices           []string `json:"pci_devices"`
}

func newPublicVMParamsResponse(params runner.PublicVMParams) *publicVMParamsResponse {
	disks := params.DisksGiB
	if disks == nil {
		disks = []int{}
	}
	pci := params.PCIDevices
	if pci == nil {
		pci = []string{}
	}
	return &publicVMParamsResponse{
		Name:                 params.Name,
		CPU:                  params.CPU,
		MemoryGiB:            params.MemoryGiB,
		DisksGiB:             disks,
		CloudImageURL:        params.CloudImageURL,
		ConsoleUser:          params.ConsoleUser,
		AutomationUser:       params.AutomationUser,
		AutomationUserPubkey: params.AutomationUserPubkey,
		PCIDevices:           pci,
	}
}

// GET /vm/{name} — returns virsh dominfo combined with creation params as JSON.
func (h *handlers) getVM(w http.ResponseWriter, r *http.Request) {
	vmName := r.PathValue("name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "virsh", "dominfo", vmName).Output()
	if err != nil {
		slog.Error("virsh dominfo failed", "vm_name", vmName, "err", err)
		writeErrorJSON(w, http.StatusInternalServerError, "virsh dominfo failed: "+err.Error())
		return
	}

	info := parseVirshDominfo(string(out))

	if params, err := h.runner.WorkspaceParams(vmName); err == nil {
		info.CreationParams = newPublicVMParamsResponse(params)
	}

	writeJSON(w, http.StatusOK, info)
}

// parseVirshDominfo parses the key: value output of virsh dominfo into a vmInfo.
//
// Example output:
//
//	Id:             2
//	Name:           intel
//	UUID:           abc-123
//	OS Type:        hvm
//	State:          running
//	CPU(s):         6
//	Max memory:     33554432 KiB
//	Used memory:    33554432 KiB
//	Persistent:     yes
//	Autostart:      disable
func parseVirshDominfo(output string) vmInfo {
	fields := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return vmInfo{
		Name:       fields["Name"],
		State:      fields["State"],
		CPUs:       fields["CPU(s)"],
		MaxMemory:  fields["Max memory"],
		UsedMemory: fields["Used memory"],
		Persistent: fields["Persistent"],
		Autostart:  fields["Autostart"],
	}
}

// virshActionResponse is returned by start and shutdown endpoints.
type virshActionResponse struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// POST /vm/{name}/start — runs virsh start.
func (h *handlers) startVM(w http.ResponseWriter, r *http.Request) {
	vmName := r.PathValue("name")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "virsh", "start", vmName).CombinedOutput()
	if err != nil {
		slog.Error("virsh start failed", "vm_name", vmName, "err", err, "output", string(out))
		writeErrorJSON(w, http.StatusInternalServerError, "virsh start failed: "+strings.TrimSpace(string(out)))
		return
	}

	slog.Info("VM started", "vm_name", vmName)
	writeJSON(w, http.StatusOK, virshActionResponse{
		Name:    vmName,
		Message: strings.TrimSpace(string(out)),
	})
}

// POST /vm/{name}/shutdown — runs virsh shutdown (graceful ACPI shutdown).
func (h *handlers) shutdownVM(w http.ResponseWriter, r *http.Request) {
	vmName := r.PathValue("name")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "virsh", "shutdown", vmName).CombinedOutput()
	if err != nil {
		slog.Error("virsh shutdown failed", "vm_name", vmName, "err", err, "output", string(out))
		writeErrorJSON(w, http.StatusInternalServerError, "virsh shutdown failed: "+strings.TrimSpace(string(out)))
		return
	}

	slog.Info("VM shutdown initiated", "vm_name", vmName)
	writeJSON(w, http.StatusOK, virshActionResponse{
		Name:    vmName,
		Message: strings.TrimSpace(string(out)),
	})
}

// healthResponse is the body returned by GET /health.
type healthResponse struct {
	Hostname  string `json:"hostname"`
	UptimeSec int64  `json:"uptime_sec"`
}

var startTime = time.Now()

// GET /health
func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Hostname:  hostname,
		UptimeSec: int64(time.Since(startTime).Seconds()),
	})
}
