// Package runner handles cloning vm-builder-core, writing terraform variable
// files, executing terraform subprocesses, and streaming their output.
package runner

import (
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
	MemoryGiB int `json:"memory_gib"`
	// vm_disk_sizes_gib — first element is root disk, remainder are data disks.
	// Max 8 disks. Default [48].
	DisksGiB []int `json:"disks_gib"`
	// vm_cloud_image_url
	CloudImageURL string `json:"cloud_image_url"`
	// vm_console_user
	ConsoleUser string `json:"console_user"`
	// vm_console_password
	ConsolePassword string `json:"console_password"`
	// vm_automation_user
	AutomationUser string `json:"automation_user"`
	// vm_automation_user_pubkey
	AutomationUserPubkey string `json:"automation_user_pubkey"`
	// pci_devices — list of PCI BDF addresses for passthrough (e.g. ["0000:01:00.0","0000:01:00.1"]).
	PCIDevices []string `json:"pci_devices"`
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
		p.PCIDevices = []string{}
	}
}

// tfvarsTemplate renders a terraform.tfvars file from a VMParams value.
// All 11 root variables from vm-builder-core are written explicitly so that
// the rendered file is self-documenting and never relies on terraform defaults.
var tfvarsTemplate = template.Must(template.New("tfvars").Funcs(template.FuncMap{
	"parseBDF": parseBDF,
}).Parse(
	`vm_name            = "{{ .Name }}"
vm_cpu_count       = {{ .CPU }}
vm_memory_size_gib = {{ .MemoryGiB }}
vm_disk_sizes_gib  = [{{ range $i, $d := .DisksGiB }}{{ if $i }}, {{ end }}{{ $d }}{{ end }}]
vm_cloud_image_url = "{{ .CloudImageURL }}"

vm_console_user     = "{{ .ConsoleUser }}"
vm_console_password = "{{ .ConsolePassword }}"

vm_automation_user        = "{{ .AutomationUser }}"
vm_automation_user_pubkey = "{{ .AutomationUserPubkey }}"

pci_devices = [{{ range $i, $d := .PCIDevices }}{{ if $i }}, {{ end }}{{ with parseBDF $d }}{ domain = {{ .Domain }}, bus = {{ .Bus }}, slot = {{ .Slot }}, function = {{ .Function }} }{{ end }}{{ end }}]
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

// ErrVMExists is returned when a create is requested for a VM whose workspace
// already exists, indicating it was previously created and not yet destroyed.
type ErrVMExists struct{ VMName string }

func (e ErrVMExists) Error() string {
	return fmt.Sprintf("VM %q already exists — delete it before recreating", e.VMName)
}

// WorkspaceExists reports whether a named workspace directory already exists
// on disk, indicating the VM was previously created and not yet destroyed.
func (r *Runner) WorkspaceExists(vmName string) bool {
	_, err := os.Stat(r.workspaceDir(vmName))
	return err == nil
}

// lockVM attempts to acquire the per-VM mutex without blocking. Returns the
// unlock function on success, or ErrVMLocked if already held.
func (r *Runner) lockVM(vmName string) (unlock func(), err error) {
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

// Create caches the cloud image, clones vm-builder-core into the VM's workspace,
// writes tfvars, then runs terraform init + apply. Blocks until complete.
// The workspace is kept after completion so terraform state persists for destroy.
// Returns accumulated command output regardless of success or failure.
func (r *Runner) Create(ctx context.Context, params VMParams) (string, error) {
	params.applyDefaults()

	unlock, err := r.lockVM(params.Name)
	if err != nil {
		return "", err
	}
	defer unlock()

	cachedURL, err := r.cacheCloudImage(ctx, params.CloudImageURL)
	if err != nil {
		return "", fmt.Errorf("cache cloud image: %w", err)
	}
	params.CloudImageURL = cachedURL

	workDir, cloneOut, err := r.prepareWorkspace(ctx, params.Name)
	if err != nil {
		return cloneOut, err
	}

	if err := r.writeTFVars(workDir, params); err != nil {
		return cloneOut, fmt.Errorf("write tfvars: %w", err)
	}

	slog.Info("terraform init", "vm_name", params.Name)
	initOut, err := r.runCmd(ctx, workDir, r.cfg.TerraformBin, "init", "-no-color")
	output := cloneOut + initOut
	if err != nil {
		return output, fmt.Errorf("terraform init: %w", err)
	}

	slog.Info("terraform apply", "vm_name", params.Name)
	applyOut, err := r.runCmd(ctx, workDir, r.cfg.TerraformBin, "apply", "-auto-approve", "-no-color")
	output += applyOut
	if err != nil {
		return output, fmt.Errorf("terraform apply: %w", err)
	}

	slog.Info("workspace kept", "path", workDir)
	return output, nil
}

// Destroy runs terraform destroy against the VM's existing workspace so that
// the state file from the original create is available. The workspace is
// removed after a successful destroy. Blocks until complete.
// Returns accumulated command output regardless of success or failure.
func (r *Runner) Destroy(ctx context.Context, vmName string) (string, error) {
	unlock, err := r.lockVM(vmName)
	if err != nil {
		return "", err
	}
	defer unlock()

	workDir := r.workspaceDir(vmName)
	if _, err := os.Stat(workDir); os.IsNotExist(err) {
		return "", fmt.Errorf("workspace not found for VM %q at %s — was it created by this agent?", vmName, workDir)
	}

	slog.Info("terraform init", "vm_name", vmName)
	initOut, err := r.runCmd(ctx, workDir, r.cfg.TerraformBin, "init", "-no-color")
	output := initOut
	if err != nil {
		return output, fmt.Errorf("terraform init: %w", err)
	}

	slog.Info("terraform destroy", "vm_name", vmName)
	destroyOut, err := r.runCmd(ctx, workDir, r.cfg.TerraformBin, "destroy", "-auto-approve", "-no-color")
	output += destroyOut
	if err != nil {
		return output, fmt.Errorf("terraform destroy: %w", err)
	}

	slog.Info("removing workspace", "path", workDir)
	if err := os.RemoveAll(workDir); err != nil {
		slog.Error("failed to remove workspace", "path", workDir, "err", err)
	}
	return output, nil
}

// workspaceDir returns the canonical workspace path for a given VM name.
func (r *Runner) workspaceDir(vmName string) string {
	return filepath.Join(r.cfg.WorkspacesDir, vmName)
}

// prepareWorkspace creates a fresh workspace directory and clones vm-builder-core
// into it. Returns ErrVMExists if a workspace already exists for this VM name.
func (r *Runner) prepareWorkspace(ctx context.Context, vmName string) (workDir string, output string, err error) {
	workDir = r.workspaceDir(vmName)

	if _, err = os.Stat(workDir); err == nil {
		return "", "", ErrVMExists{VMName: vmName}
	}

	if err = os.MkdirAll(workDir, 0o750); err != nil {
		return "", "", fmt.Errorf("create workspace dir: %w", err)
	}

	slog.Info("cloning core repo", "url", r.cfg.CoreRepoURL, "dir", workDir)
	output, err = r.runCmd(ctx, workDir, "git", "clone", "--depth=1", r.cfg.CoreRepoURL, ".")
	if err != nil {
		os.RemoveAll(workDir)
		return "", output, fmt.Errorf("git clone: %w", err)
	}

	return workDir, output, nil
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

// runCmd runs a single command and returns its combined stdout+stderr output.
// Returns an error if the command exits non-zero.
func (r *Runner) runCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	slog.Info("running command", "cmd", name, "args", args, "dir", dir)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// cacheCloudImage returns the cloud image URL to use for terraform. For
// http/https URLs the image is downloaded into the cache directory and a
// file:// URL pointing to the cached copy is returned. file:// URLs are
// returned as-is — the local path is used directly with no copying.
// Concurrent downloads of the same remote URL are serialized.
func (r *Runner) cacheCloudImage(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse cloud image URL: %w", err)
	}

	// Local file — use as-is, no caching needed.
	if u.Scheme == "file" {
		return rawURL, nil
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported cloud image URL scheme %q", u.Scheme)
	}

	filename := filepath.Base(u.Path)
	if filename == "" || filename == "." {
		return "", fmt.Errorf("cannot determine filename from cloud image URL %q", rawURL)
	}

	cachePath := filepath.Join(r.cfg.CloudImageCacheDir, filename)

	// Fast path: already cached — no lock needed for a stat check.
	if _, err := os.Stat(cachePath); err == nil {
		slog.Info("cloud image cache hit", "path", cachePath)
		return "file://" + cachePath, nil
	}

	// Serialize concurrent downloads of the same image.
	r.cacheMu.Lock()
	l, ok := r.imageLocks[filename]
	if !ok {
		l = &sync.Mutex{}
		r.imageLocks[filename] = l
	}
	r.cacheMu.Unlock()

	l.Lock()
	defer l.Unlock()

	// Re-check after acquiring the lock — another goroutine may have
	// already populated the cache.
	if _, err := os.Stat(cachePath); err == nil {
		slog.Info("cloud image cache hit", "path", cachePath)
		return "file://" + cachePath, nil
	}

	if err := os.MkdirAll(r.cfg.CloudImageCacheDir, 0o750); err != nil {
		return "", fmt.Errorf("create cloud image cache dir: %w", err)
	}

	slog.Info("caching cloud image", "url", rawURL, "path", cachePath)
	if err := cacheFromHTTP(ctx, rawURL, cachePath); err != nil {
		return "", fmt.Errorf("cache cloud image: %w", err)
	}

	slog.Info("cloud image cached", "path", cachePath)
	return "file://" + cachePath, nil
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
	PCIDevices           []string `json:"pciDevices"`
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
		PCIDevices:           parseBDFObjectList(kv["pci_devices"]),
	}
	return p
}

// bdfAddr holds the four numeric components of a PCI BDF address.
type bdfAddr struct {
	Domain, Bus, Slot, Function uint64
}

// parseBDF parses a canonical PCI BDF string ("0000:01:00.0") into its
// four numeric components. Returns nil if the string is malformed.
func parseBDF(addr string) *bdfAddr {
	// Expected format: DDDD:BB:SS.F  (all hex)
	parts := strings.SplitN(addr, ":", 3)
	if len(parts) != 3 {
		return nil
	}
	sf := strings.SplitN(parts[2], ".", 2)
	if len(sf) != 2 {
		return nil
	}
	parse := func(s string) (uint64, error) { return strconv.ParseUint(s, 16, 64) }
	d, err1 := parse(parts[0])
	b, err2 := parse(parts[1])
	s, err3 := parse(sf[0])
	f, err4 := parse(sf[1])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return nil
	}
	return &bdfAddr{Domain: d, Bus: b, Slot: s, Function: f}
}

// parseBDFObjectList parses the tfvars object-list representation written by
// tfvarsTemplate back into BDF strings. It handles the format:
//
//	[{ domain = 0, bus = 1, slot = 0, function = 0 }, ...]
func parseBDFObjectList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	// Split on "}" to get individual objects, then re-parse each.
	var out []string
	for _, chunk := range strings.Split(s, "}") {
		chunk = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(chunk), ","))
		chunk = strings.TrimPrefix(chunk, "{")
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		kv := map[string]uint64{}
		for _, field := range strings.Split(chunk, ",") {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err != nil {
				continue
			}
			kv[strings.TrimSpace(k)] = n
		}
		out = append(out, fmt.Sprintf("%04x:%02x:%02x.%x",
			kv["domain"], kv["bus"], kv["slot"], kv["function"]))
	}
	return out
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
