// Package runner handles cloning vm-builder-core, writing terraform variable
// files, executing terraform subprocesses, and streaming their output.
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"github.com/tlhakhan/vm-builder-agent/jobs"
)

// Config holds the parameters the runner needs at startup.
type Config struct {
	// CoreRepoURL is the git URL for vm-builder-core.
	CoreRepoURL string
	// TerraformBin is the path (or name) of the terraform/opentofu binary.
	TerraformBin string
	// WorkspacesDir is the root directory under which per-VM terraform
	// workspaces are created and persisted (e.g. /var/lib/vm-builder-agent/workspaces).
	WorkspacesDir string
	// CloudImageCacheDir is where cloud images are cached so that subsequent
	// VM creates with the same image skip re-downloading.
	CloudImageCacheDir string
}

// VMParams is the decoded request body for a VM create operation.
// Field names and types mirror the root variables.tf in vm-builder-core.
type VMParams struct {
	// vm_name
	Name string `json:"name"`
	// vm_cpu_count (default 2)
	CPU int `json:"cpu"`
	// vm_memory_size_gib (default 4)
	MemoryGiB int `json:"memoryGib"`
	// vm_disk_sizes_gib — first element is root disk, remainder are data disks.
	// Max 8 disks. Default [48].
	DisksGiB []int `json:"disksGib"`
	// vm_cloud_image_url
	CloudImageURL string `json:"cloudImageUrl"`
	// vm_console_user
	ConsoleUser string `json:"consoleUser"`
	// vm_console_password
	ConsolePassword string `json:"consolePassword"`
	// vm_automation_user
	AutomationUser string `json:"automationUser"`
	// vm_automation_user_pubkey
	AutomationUserPubkey string `json:"automationUserPubkey"`
	// pci_devices — list of PCI bus numbers for passthrough (e.g. GPU).
	PCIDevices []int `json:"pciDevices"`
}

// applyDefaults fills in zero values with the same defaults as variables.tf so
// that every field in the rendered tfvars is explicit.
func (p *VMParams) applyDefaults() {
	if p.CPU == 0 {
		p.CPU = 2
	}
	if p.MemoryGiB == 0 {
		p.MemoryGiB = 4
	}
	if len(p.DisksGiB) == 0 {
		p.DisksGiB = []int{48}
	}
	if p.PCIDevices == nil {
		p.PCIDevices = []int{}
	}
}

// tfvarsTemplate renders a terraform.tfvars file from a VMParams value.
// All 11 root variables from vm-builder-core are written explicitly so that
// the rendered file is self-documenting and never relies on terraform defaults.
var tfvarsTemplate = template.Must(template.New("tfvars").Parse(
	`vm_name            = "{{ .Name }}"
vm_cpu_count       = {{ .CPU }}
vm_memory_size_gib = {{ .MemoryGiB }}
vm_disk_sizes_gib  = [{{ range $i, $d := .DisksGiB }}{{ if $i }}, {{ end }}{{ $d }}{{ end }}]
vm_cloud_image_url = "{{ .CloudImageURL }}"

vm_console_user     = "{{ .ConsoleUser }}"
vm_console_password = "{{ .ConsolePassword }}"

vm_automation_user        = "{{ .AutomationUser }}"
vm_automation_user_pubkey = "{{ .AutomationUserPubkey }}"

pci_devices               = [{{ range $i, $d := .PCIDevices }}{{ if $i }}, {{ end }}{{ $d }}{{ end }}]
`))

// Runner executes terraform workflows on behalf of the HTTP handlers.
type Runner struct {
	cfg Config

	mu      sync.Mutex
	vmLocks map[string]*sync.Mutex

	cacheMu    sync.Mutex
	imageLocks map[string]*sync.Mutex
}

// New returns a Runner with the given config.
func New(cfg Config) *Runner {
	return &Runner{
		cfg:        cfg,
		vmLocks:    make(map[string]*sync.Mutex),
		imageLocks: make(map[string]*sync.Mutex),
	}
}

// ErrVMLocked is returned when a request arrives for a VM that already has an
// operation in flight.
type ErrVMLocked struct{ VMName string }

func (e ErrVMLocked) Error() string {
	return fmt.Sprintf("VM %q is already being modified by another operation", e.VMName)
}

// WorkspaceExists reports whether a named workspace directory already exists
// on disk, indicating the VM was previously created and not yet destroyed.
func (r *Runner) WorkspaceExists(vmName string) bool {
	_, err := os.Stat(r.workspaceDir(vmName))
	return err == nil
}

// LockVM attempts to acquire the per-VM mutex without blocking. Call this in
// the HTTP handler before writing any response headers so that a 409 can still
// be returned. Returns the unlock function on success, or ErrVMLocked if the
// VM already has an operation in flight.
func (r *Runner) LockVM(vmName string) (unlock func(), err error) {
	r.mu.Lock()
	l, ok := r.vmLocks[vmName]
	if !ok {
		l = &sync.Mutex{}
		r.vmLocks[vmName] = l
	}
	r.mu.Unlock()

	if !l.TryLock() {
		return nil, ErrVMLocked{VMName: vmName}
	}
	return l.Unlock, nil
}

// Create clones vm-builder-core into the VM's named workspace, writes tfvars,
// then runs terraform init + apply. The workspace is kept after completion so
// that terraform state persists for a future destroy.
func (r *Runner) Create(ctx context.Context, job *jobs.Job, params VMParams, w io.Writer) error {
	params.applyDefaults()

	cachedURL, err := r.cacheCloudImage(ctx, job, params.CloudImageURL, w)
	if err != nil {
		return fmt.Errorf("cache cloud image: %w", err)
	}
	params.CloudImageURL = cachedURL

	workDir, err := r.prepareWorkspace(ctx, job, params.Name, w)
	if err != nil {
		return err
	}
	// No cleanup on create — workspace is kept for state persistence.

	if err := r.writeTFVars(workDir, params); err != nil {
		return fmt.Errorf("write tfvars: %w", err)
	}

	job.SetPhase(jobs.PhaseInit)
	r.emit(job, w, "=== terraform init ===\n")
	if err := r.runCmd(ctx, job, w, workDir, r.cfg.TerraformBin, "init", "-no-color"); err != nil {
		return fmt.Errorf("terraform init: %w", err)
	}

	job.SetPhase(jobs.PhaseApplying)
	r.emit(job, w, "=== terraform apply ===\n")
	if err := r.runCmd(ctx, job, w, workDir, r.cfg.TerraformBin,
		"apply", "-auto-approve", "-no-color"); err != nil {
		return fmt.Errorf("terraform apply: %w", err)
	}

	r.emit(job, w, fmt.Sprintf("=== workspace kept at %s ===\n", workDir))
	return nil
}

// Destroy runs terraform destroy against the VM's existing named workspace so
// that the state file from the original create is available. The workspace is
// removed after a successful destroy.
func (r *Runner) Destroy(ctx context.Context, job *jobs.Job, vmName string, w io.Writer) error {
	workDir := r.workspaceDir(vmName)

	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		return fmt.Errorf("workspace not found for VM %q at %s — was it created by this agent?", vmName, workDir)
	}

	job.SetPhase(jobs.PhaseInit)
	r.emit(job, w, fmt.Sprintf("=== using workspace %s ===\n", workDir))
	r.emit(job, w, "=== terraform init ===\n")
	if err := r.runCmd(ctx, job, w, workDir, r.cfg.TerraformBin, "init", "-no-color"); err != nil {
		return fmt.Errorf("terraform init: %w", err)
	}

	job.SetPhase(jobs.PhaseDestroying)
	r.emit(job, w, "=== terraform destroy ===\n")
	if err := r.runCmd(ctx, job, w, workDir, r.cfg.TerraformBin,
		"destroy", "-auto-approve", "-no-color"); err != nil {
		return fmt.Errorf("terraform destroy: %w", err)
	}

	r.removeWorkspace(workDir, job)
	return nil
}

// cacheCloudImage ensures the cloud image at rawURL is present in the cache
// directory and returns a file:// URL pointing to the cached copy. If the image
// is already cached the function returns immediately. Concurrent requests for
// the same image are serialized so the image is only downloaded once.
func (r *Runner) cacheCloudImage(ctx context.Context, job *jobs.Job, rawURL string, w io.Writer) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse cloud image URL: %w", err)
	}

	filename := filepath.Base(u.Path)
	if filename == "" || filename == "." {
		return "", fmt.Errorf("cannot determine filename from cloud image URL %q", rawURL)
	}

	cachePath := filepath.Join(r.cfg.CloudImageCacheDir, filename)

	// Fast path: already cached — no lock needed for a stat check.
	if _, err := os.Stat(cachePath); err == nil {
		r.emit(job, w, fmt.Sprintf("=== cloud image cache hit: %s ===\n", cachePath))
		return "file://" + cachePath, nil
	}

	// Serialize concurrent downloads/copies of the same image.
	r.cacheMu.Lock()
	l, ok := r.imageLocks[filename]
	if !ok {
		l = &sync.Mutex{}
		r.imageLocks[filename] = l
	}
	r.cacheMu.Unlock()

	l.Lock()
	defer l.Unlock()

	// Re-check after acquiring the lock — a concurrent goroutine may have
	// already populated the cache.
	if _, err := os.Stat(cachePath); err == nil {
		r.emit(job, w, fmt.Sprintf("=== cloud image cache hit: %s ===\n", cachePath))
		return "file://" + cachePath, nil
	}

	if err := os.MkdirAll(r.cfg.CloudImageCacheDir, 0o750); err != nil {
		return "", fmt.Errorf("create cloud image cache dir: %w", err)
	}

	r.emit(job, w, fmt.Sprintf("=== caching cloud image %s → %s ===\n", rawURL, cachePath))

	switch u.Scheme {
	case "file":
		if err := cacheFromFile(u.Path, cachePath); err != nil {
			return "", fmt.Errorf("cache cloud image from file: %w", err)
		}
	case "http", "https":
		if err := cacheFromHTTP(ctx, rawURL, cachePath); err != nil {
			return "", fmt.Errorf("cache cloud image from HTTP: %w", err)
		}
	default:
		return "", fmt.Errorf("unsupported cloud image URL scheme %q", u.Scheme)
	}

	r.emit(job, w, fmt.Sprintf("=== cloud image cached at %s ===\n", cachePath))
	return "file://" + cachePath, nil
}

// cacheFromFile copies (or hard-links) a local file into the cache using an
// atomic temp-file + rename pattern so a partial write is never visible.
func cacheFromFile(src, dst string) error {
	tmp := dst + ".tmp"

	// Prefer a hard link — instant and uses no extra space when on the same
	// filesystem.
	if err := os.Link(src, tmp); err == nil {
		return os.Rename(tmp, dst)
	}

	// Fall back to a full copy (cross-device or unsupported filesystem).
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// cacheFromHTTP downloads a remote image into the cache using an atomic
// temp-file + rename pattern so a partial download is never visible.
func cacheFromHTTP(ctx context.Context, rawURL, dst string) error {
	tmp := dst + ".tmp"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, rawURL)
	}

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// workspaceDir returns the canonical workspace path for a given VM name.
func (r *Runner) workspaceDir(vmName string) string {
	return filepath.Join(r.cfg.WorkspacesDir, vmName)
}

// ErrVMExists is returned when a create is requested for a VM whose workspace
// already exists, indicating it was previously created and not yet destroyed.
type ErrVMExists struct{ VMName string }

func (e ErrVMExists) Error() string {
	return fmt.Sprintf("VM %q already exists — delete it before recreating", e.VMName)
}

// prepareWorkspace creates a fresh workspace directory for the VM and clones
// vm-builder-core into it. Returns ErrVMExists if a workspace already exists
// for this VM name.
func (r *Runner) prepareWorkspace(ctx context.Context, job *jobs.Job, vmName string, w io.Writer) (string, error) {
	workDir := r.workspaceDir(vmName)

	if _, err := os.Stat(workDir); err == nil {
		return "", ErrVMExists{VMName: vmName}
	}

	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return "", fmt.Errorf("create workspace dir: %w", err)
	}

	job.SetPhase(jobs.PhaseCloning)
	r.emit(job, w, fmt.Sprintf("=== cloning %s into %s ===\n", r.cfg.CoreRepoURL, workDir))

	if err := r.runCmd(ctx, job, w, workDir, "git", "clone", "--depth=1", r.cfg.CoreRepoURL, "."); err != nil {
		os.RemoveAll(workDir)
		return "", fmt.Errorf("git clone: %w", err)
	}

	return workDir, nil
}

// removeWorkspace deletes the workspace directory after a successful destroy.
func (r *Runner) removeWorkspace(workDir string, job *jobs.Job) {
	slog.Info("removing workspace", "job_id", job.ID, "path", workDir)
	if err := os.RemoveAll(workDir); err != nil {
		slog.Error("failed to remove workspace", "job_id", job.ID, "path", workDir, "err", err)
	}
}

// writeTFVars renders the tfvars template and writes it to the work dir.
func (r *Runner) writeTFVars(workDir string, params VMParams) error {
	path := filepath.Join(workDir, "terraform.tfvars")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tfvarsTemplate.Execute(f, params)
}

// runCmd runs a single command, streaming its combined stdout+stderr to w and
// the job log line by line. Returns an error if the command exits non-zero.
//
// stdout and stderr are merged via an io.Pipe so that both streams appear in
// the correct order without interleaving issues from separate goroutines.
func (r *Runner) runCmd(ctx context.Context, job *jobs.Job, w io.Writer, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	slog.Info("running command", "job_id", job.ID, "cmd", name, "args", args, "dir", dir)

	// Merge stdout and stderr into a single pipe.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("start %s: %w", name, err)
	}

	// Close the write end once the process exits so the scanner sees EOF.
	waitErr := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		waitErr <- err
	}()

	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		r.emit(job, w, scanner.Text()+"\n")
	}
	pr.Close()

	if err := <-waitErr; err != nil {
		return fmt.Errorf("%s exited: %w", name, err)
	}
	return nil
}

// emit writes a line to both the HTTP response writer and the job's log buffer,
// then flushes the response if the writer supports it.
func (r *Runner) emit(job *jobs.Job, w io.Writer, line string) {
	job.AppendLog(line)
	if _, err := io.WriteString(w, line); err != nil {
		slog.Warn("failed to write to response", "job_id", job.ID, "err", err)
	}
	if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}
}

// PublicVMParams is a view of VMParams with sensitive fields omitted, safe for
// inclusion in API responses.
type PublicVMParams struct {
	Name                 string `json:"name"`
	CPU                  int    `json:"cpu"`
	MemoryGiB            int    `json:"memoryGib"`
	DisksGiB             []int  `json:"disksGib"`
	CloudImageURL        string `json:"cloudImageUrl"`
	ConsoleUser          string `json:"consoleUser"`
	AutomationUser       string `json:"automationUser"`
	AutomationUserPubkey string `json:"automationUserPubkey"`
	PCIDevices           []int  `json:"pciDevices"`
}

// WorkspaceParams reads the terraform.tfvars from the named VM's workspace and
// returns the non-sensitive creation parameters. Returns an error if the
// workspace does not exist or the file cannot be parsed.
func (r *Runner) WorkspaceParams(vmName string) (PublicVMParams, error) {
	path := filepath.Join(r.workspaceDir(vmName), "terraform.tfvars")
	data, err := os.ReadFile(path)
	if err != nil {
		return PublicVMParams{}, fmt.Errorf("read tfvars: %w", err)
	}
	return parseTFVars(string(data)), nil
}

// parseTFVars parses the subset of HCL-style key = value assignments written
// by tfvarsTemplate. Handles quoted strings, bare integers, and integer lists.
// The console_password field is intentionally never populated.
func parseTFVars(content string) PublicVMParams {
	kv := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}

	p := PublicVMParams{
		Name:                 unquote(kv["vm_name"]),
		CPU:                  parseInt(kv["vm_cpu_count"]),
		MemoryGiB:            parseInt(kv["vm_memory_size_gib"]),
		DisksGiB:             parseIntList(kv["vm_disk_sizes_gib"]),
		CloudImageURL:        unquote(kv["vm_cloud_image_url"]),
		ConsoleUser:          unquote(kv["vm_console_user"]),
		AutomationUser:       unquote(kv["vm_automation_user"]),
		AutomationUserPubkey: unquote(kv["vm_automation_user_pubkey"]),
		PCIDevices:           parseIntList(kv["pci_devices"]),
	}
	return p
}

// unquote strips surrounding double-quotes from a tfvars string value.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// parseInt parses a bare integer tfvars value, returning 0 on failure.
func parseInt(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// parseIntList parses a tfvars list literal such as "[192]" or "[3, 4]" into
// a []int. Returns an empty slice if the value is absent or malformed.
func parseIntList(s string) []int {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	s = strings.TrimSpace(s)
	if s == "" {
		return []int{}
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out = append(out, n)
		}
	}
	return out
}
