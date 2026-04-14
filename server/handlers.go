package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

func writeErrorJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type jobCreatedResponse struct {
	JobID string `json:"job_id"`
}

type jobStatusResponse struct {
	JobID     string     `json:"job_id"`
	VMName    string     `json:"vm_name,omitempty"`
	Action    string     `json:"action,omitempty"`
	Status    string     `json:"status"`
	Log       string     `json:"log"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	Error     string     `json:"error,omitempty"`
	ErrorCode string     `json:"error_code,omitempty"`
}

func newJobStatusResponse(job jobs.JobSnapshot) jobStatusResponse {
	status := "running"
	switch job.Phase {
	case jobs.PhaseDone:
		status = "done"
	case jobs.PhaseFailed:
		status = "failed"
	}

	return jobStatusResponse{
		JobID:     job.ID,
		VMName:    job.VMName,
		Action:    job.Action,
		Status:    status,
		Log:       job.Log,
		StartTime: job.StartTime,
		EndTime:   job.EndTime,
		Error:     job.Err,
		ErrorCode: job.ErrorCode,
	}
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

	if h.runner.WorkspaceExists(params.Name) {
		writeErrorJSON(w, http.StatusConflict, runner.ErrVMExists{VMName: params.Name}.Error())
		return
	}

	unlock, err := h.runner.LockVM(params.Name)
	if err != nil {
		writeErrorJSON(w, http.StatusConflict, err.Error())
		return
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	id, err := generateID()
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to generate job id")
		return
	}

	job := &jobs.Job{
		ID:        id,
		VMName:    params.Name,
		Action:    "create",
		Phase:     jobs.PhaseCloning,
		StartTime: time.Now(),
	}
	h.tracker.Add(job)

	slog.Info("create VM job started", "job_id", id, "vm_name", params.Name)
	jobCtx := context.WithoutCancel(r.Context())
	go func(ctx context.Context, job *jobs.Job, params runner.VMParams) {
		defer unlock()
		err := h.runner.Create(ctx, job, params, io.Discard)

		if err != nil {
			var exists runner.ErrVMExists
			if errors.As(err, &exists) {
				job.FinishWithCode(err, jobs.ErrorCodeDuplicate)
				slog.Warn("create rejected: VM already exists", "job_id", id, "vm_name", params.Name)
			} else {
				job.FinishWithCode(err, jobs.ErrorCodeCreateFailed)
				slog.Error("create VM job failed", "job_id", id, "vm_name", params.Name, "err", err)
			}
			return
		}
		job.Finish(nil)
		slog.Info("create VM job completed", "job_id", id, "vm_name", params.Name)
	}(jobCtx, job, params)
	locked = false

	writeJSON(w, http.StatusOK, jobCreatedResponse{JobID: id})
}

// DELETE /vm/{name}
func (h *handlers) deleteVM(w http.ResponseWriter, r *http.Request) {
	vmName := r.PathValue("name")
	if vmName == "" {
		writeErrorJSON(w, http.StatusBadRequest, "vm name is required")
		return
	}

	unlock, err := h.runner.LockVM(vmName)
	if err != nil {
		writeErrorJSON(w, http.StatusConflict, err.Error())
		return
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	id, err := generateID()
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, "failed to generate job id")
		return
	}

	job := &jobs.Job{
		ID:        id,
		VMName:    vmName,
		Action:    "delete",
		Phase:     jobs.PhaseCloning,
		StartTime: time.Now(),
	}
	h.tracker.Add(job)

	slog.Info("delete VM job started", "job_id", id, "vm_name", vmName)
	jobCtx := context.WithoutCancel(r.Context())
	go func(ctx context.Context, job *jobs.Job, vmName string) {
		defer unlock()
		err := h.runner.Destroy(ctx, job, vmName, io.Discard)

		if err != nil {
			job.FinishWithCode(err, jobs.ErrorCodeDeleteFailed)
			slog.Error("delete VM job failed", "job_id", id, "vm_name", vmName, "err", err)
			return
		}
		job.Finish(nil)
		slog.Info("delete VM job completed", "job_id", id, "vm_name", vmName)
	}(jobCtx, job, vmName)
	locked = false

	writeJSON(w, http.StatusOK, jobCreatedResponse{JobID: id})
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
	ID         string `json:"id"`
	UUID       string `json:"uuid"`
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
	PCIDevices           []int  `json:"pci_devices"`
}

func newPublicVMParamsResponse(params runner.PublicVMParams) *publicVMParamsResponse {
	disks := params.DisksGiB
	if disks == nil {
		disks = []int{}
	}
	pci := params.PCIDevices
	if pci == nil {
		pci = []int{}
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
	OK      bool   `json:"ok"`
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
		OK:      true,
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
		OK:      true,
		Name:    vmName,
		Message: strings.TrimSpace(string(out)),
	})
}

// GET /jobs/{id}
func (h *handlers) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := h.tracker.Get(id)
	if !ok {
		writeErrorJSON(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, newJobStatusResponse(job.Snapshot()))
}

// healthResponse is the body returned by GET /health.
type healthResponse struct {
	Hostname   string `json:"hostname"`
	UptimeSec  int64  `json:"uptime_sec"`
	ActiveJobs int    `json:"active_jobs"`
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
