package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tlhakhan/vm-builder-agent/jobs"
	"github.com/tlhakhan/vm-builder-agent/runner"
)

type handlers struct {
	tracker *jobs.Tracker
	runner  *runner.Runner
}

// generateID returns a random 8-byte hex string suitable for job IDs.
func generateID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "err", err)
	}
}

// POST /vm/create
func (h *handlers) createVM(w http.ResponseWriter, r *http.Request) {
	var params runner.VMParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if params.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	if h.runner.WorkspaceExists(params.Name) {
		http.Error(w, runner.ErrVMExists{VMName: params.Name}.Error(), http.StatusConflict)
		return
	}

	unlock, err := h.runner.LockVM(params.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer unlock()

	id, err := generateID()
	if err != nil {
		http.Error(w, "failed to generate job id", http.StatusInternalServerError)
		return
	}

	job := &jobs.Job{
		ID:        id,
		VMName:    params.Name,
		Phase:     jobs.PhaseCloning,
		StartTime: time.Now(),
	}
	h.tracker.Add(job)

	slog.Info("create VM job started", "job_id", id, "vm_name", params.Name)

	// Stream the terraform output back to the caller.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Job-ID", id)
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	fmt.Fprintf(w, "job_id: %s\n\n", id)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	err = h.runner.Create(r.Context(), job, params, w)
	job.Finish(err)

	if err != nil {
		var exists runner.ErrVMExists
		if errors.As(err, &exists) {
			slog.Warn("create rejected: VM already exists", "job_id", id, "vm_name", params.Name)
		} else {
			slog.Error("create VM job failed", "job_id", id, "vm_name", params.Name, "err", err)
		}
		fmt.Fprintf(w, "\nERROR: %s\n", err)
	} else {
		slog.Info("create VM job completed", "job_id", id, "vm_name", params.Name)
		fmt.Fprintf(w, "\nDONE\n")
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// DELETE /vm/{name}
func (h *handlers) deleteVM(w http.ResponseWriter, r *http.Request) {
	vmName := r.PathValue("name")
	if vmName == "" {
		http.Error(w, "vm name is required", http.StatusBadRequest)
		return
	}

	unlock, err := h.runner.LockVM(vmName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer unlock()

	id, err := generateID()
	if err != nil {
		http.Error(w, "failed to generate job id", http.StatusInternalServerError)
		return
	}

	job := &jobs.Job{
		ID:        id,
		VMName:    vmName,
		Phase:     jobs.PhaseCloning,
		StartTime: time.Now(),
	}
	h.tracker.Add(job)

	slog.Info("delete VM job started", "job_id", id, "vm_name", vmName)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Job-ID", id)
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	fmt.Fprintf(w, "job_id: %s\n\n", id)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	err = h.runner.Destroy(r.Context(), job, vmName, w)
	job.Finish(err)

	if err != nil {
		slog.Error("delete VM job failed", "job_id", id, "vm_name", vmName, "err", err)
		fmt.Fprintf(w, "\nERROR: %s\n", err)
	} else {
		slog.Info("delete VM job completed", "job_id", id, "vm_name", vmName)
		fmt.Fprintf(w, "\nDONE\n")
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
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
		http.Error(w, "virsh list failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	vms := parseVirshList(string(out))
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
	ID         string `json:"id"`
	UUID       string `json:"uuid"`
	State      string `json:"state"`
	CPUs       string `json:"cpus"`
	MaxMemory  string `json:"maxMemory"`
	UsedMemory string `json:"usedMemory"`
	Persistent string `json:"persistent"`
	Autostart  string `json:"autostart"`
	// from terraform.tfvars (omitted when workspace is absent)
	CreationParams *runner.PublicVMParams `json:"creationParams,omitempty"`
}

// GET /vm/{name} — returns virsh dominfo combined with creation params as JSON.
func (h *handlers) getVM(w http.ResponseWriter, r *http.Request) {
	vmName := r.PathValue("name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "virsh", "dominfo", vmName).Output()
	if err != nil {
		slog.Error("virsh dominfo failed", "vm_name", vmName, "err", err)
		http.Error(w, "virsh dominfo failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	info := parseVirshDominfo(string(out))

	if params, err := h.runner.WorkspaceParams(vmName); err == nil {
		info.CreationParams = &params
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
		ID:         fields["Id"],
		UUID:       fields["UUID"],
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
	VMName  string `json:"vmName"`
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
		http.Error(w, "virsh start failed: "+strings.TrimSpace(string(out)), http.StatusInternalServerError)
		return
	}

	slog.Info("VM started", "vm_name", vmName)
	writeJSON(w, http.StatusOK, virshActionResponse{
		VMName:  vmName,
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
		http.Error(w, "virsh shutdown failed: "+strings.TrimSpace(string(out)), http.StatusInternalServerError)
		return
	}

	slog.Info("VM shutdown initiated", "vm_name", vmName)
	writeJSON(w, http.StatusOK, virshActionResponse{
		VMName:  vmName,
		Message: strings.TrimSpace(string(out)),
	})
}

// GET /jobs/{id}
func (h *handlers) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := h.tracker.Get(id)
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, job.Snapshot())
}

// healthResponse is the body returned by GET /health.
type healthResponse struct {
	Hostname   string `json:"hostname"`
	UptimeSec  int64  `json:"uptimeSec"`
	ActiveJobs int    `json:"activeJobs"`
}

var startTime = time.Now()

// GET /health
func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Hostname:   hostname,
		UptimeSec:  int64(time.Since(startTime).Seconds()),
		ActiveJobs: h.tracker.ActiveCount(),
	})
}
