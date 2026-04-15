package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tlhakhan/vm-builder-agent/runner"
)

func TestCreateVMReturnsOK(t *testing.T) {
	h, env := newTestHandlers(t)

	body := bytes.NewBufferString(`{
		"name":"vm-create",
		"cpu":2,
		"memory_gib":4,
		"disks_gib":[48],
		"cloud_image_url":"file://` + env.imagePath + `",
		"console_user":"ubuntu",
		"console_password":"secret",
		"automation_user":"auto",
		"automation_user_pubkey":"ssh-rsa test",
		"pci_devices":[]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/vm/create", body)
	rec := httptest.NewRecorder()

	h.createVM(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", got, http.StatusOK, rec.Body.String())
	}
	assertJSONContentType(t, rec)

	var resp struct {
		Name string `json:"name"`
	}
	decodeJSON(t, rec, &resp)
	if resp.Name != "vm-create" {
		t.Fatalf("name = %q, want %q", resp.Name, "vm-create")
	}
}

func TestDeleteVMReturnsOK(t *testing.T) {
	h, env := newTestHandlers(t)

	vmName := "vm-delete"
	if err := os.MkdirAll(filepath.Join(env.workspacesDir, vmName), 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/vm/"+vmName, nil)
	req.SetPathValue("name", vmName)
	rec := httptest.NewRecorder()

	h.deleteVM(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", got, http.StatusOK, rec.Body.String())
	}
	assertJSONContentType(t, rec)

	var resp struct {
		Name string `json:"name"`
	}
	decodeJSON(t, rec, &resp)
	if resp.Name != vmName {
		t.Fatalf("name = %q, want %q", resp.Name, vmName)
	}

	if _, err := os.Stat(filepath.Join(env.workspacesDir, vmName)); !os.IsNotExist(err) {
		t.Fatalf("expected workspace to be removed, stat err = %v", err)
	}
}

func TestListVMsReturnsEmptyArrayNotNull(t *testing.T) {
	h, env := newTestHandlers(t)
	if err := os.WriteFile(env.virshListPath, []byte(" Id   Name   State\n--------------------\n"), 0o644); err != nil {
		t.Fatalf("write virsh list fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/vm", nil)
	rec := httptest.NewRecorder()

	h.listVMs(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	assertJSONContentType(t, rec)
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("body = %q, want []", rec.Body.String())
	}

	var resp []map[string]any
	decodeJSON(t, rec, &resp)
	if len(resp) != 0 {
		t.Fatalf("len(resp) = %d, want 0", len(resp))
	}
}

func TestGetVMUsesSnakeCaseFields(t *testing.T) {
	h, env := newTestHandlers(t)

	vmName := "vm-info"
	workspaceDir := filepath.Join(env.workspacesDir, vmName)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	tfvars := `vm_name            = "vm-info"
vm_cpu_count       = 4
vm_memory_size_gib = 8
vm_disk_sizes_gib  = [48, 64]
vm_cloud_image_url = "file:///tmp/image.qcow2"

vm_console_user     = "ubuntu"
vm_console_password = "secret"

vm_automation_user        = "auto"
vm_automation_user_pubkey = "ssh-rsa test"

pci_devices               = [1, 2]
`
	if err := os.WriteFile(filepath.Join(workspaceDir, "terraform.tfvars"), []byte(tfvars), 0o644); err != nil {
		t.Fatalf("write tfvars: %v", err)
	}
	dominfo := "Id:             2\nName:           vm-info\nUUID:           abc-123\nState:          running\nCPU(s):         4\nMax memory:     8388608 KiB\nUsed memory:    4194304 KiB\nPersistent:     yes\nAutostart:      disable\n"
	if err := os.WriteFile(env.virshDominfoPath, []byte(dominfo), 0o644); err != nil {
		t.Fatalf("write dominfo fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/vm/"+vmName, nil)
	req.SetPathValue("name", vmName)
	rec := httptest.NewRecorder()

	h.getVM(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	assertJSONContentType(t, rec)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if _, ok := resp["max_memory"]; !ok {
		t.Fatalf("expected max_memory field in response: %v", resp)
	}
	if _, ok := resp["used_memory"]; !ok {
		t.Fatalf("expected used_memory field in response: %v", resp)
	}
	params, ok := resp["creation_params"].(map[string]any)
	if !ok {
		t.Fatalf("expected creation_params object in response: %v", resp)
	}
	if _, ok := params["memory_gib"]; !ok {
		t.Fatalf("expected memory_gib in creation_params: %v", params)
	}
	if _, ok := params["cloud_image_url"]; !ok {
		t.Fatalf("expected cloud_image_url in creation_params: %v", params)
	}
}

func TestStartAndShutdownReturnJSONSuccessObjects(t *testing.T) {
	h, _ := newTestHandlers(t)

	for _, tc := range []struct {
		name   string
		path   string
		call   func(*handlers, http.ResponseWriter, *http.Request)
		wantOK string
	}{
		{name: "start", path: "/vm/vm-1/start", call: (*handlers).startVM, wantOK: "started"},
		{name: "shutdown", path: "/vm/vm-1/shutdown", call: (*handlers).shutdownVM, wantOK: "shutdown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.SetPathValue("name", "vm-1")
			rec := httptest.NewRecorder()

			tc.call(h, rec, req)

			if got := rec.Code; got != http.StatusOK {
				t.Fatalf("status = %d, want %d", got, http.StatusOK)
			}
			assertJSONContentType(t, rec)

			var resp struct {
				Name    string `json:"name"`
				Message string `json:"message"`
			}
			decodeJSON(t, rec, &resp)
			if resp.Name != "vm-1" {
				t.Fatalf("unexpected response: %+v", resp)
			}
			if !strings.Contains(strings.ToLower(resp.Message), tc.wantOK) {
				t.Fatalf("message = %q, want substring %q", resp.Message, tc.wantOK)
			}
		})
	}
}

func TestErrorResponsesAreJSON(t *testing.T) {
	h, _ := newTestHandlers(t)

	t.Run("create invalid body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/vm/create", strings.NewReader("{"))
		rec := httptest.NewRecorder()

		h.createVM(rec, req)

		if got := rec.Code; got != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
		}
		assertJSONContentType(t, rec)

		var resp map[string]string
		decodeJSON(t, rec, &resp)
		if resp["error"] == "" {
			t.Fatalf("expected error message, got %v", resp)
		}
	})
}

type testEnv struct {
	workspacesDir    string
	imagePath        string
	virshListPath    string
	virshDominfoPath string
}

func newTestHandlers(t *testing.T) (*handlers, testEnv) {
	t.Helper()

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	repoDir := filepath.Join(tmp, "repo")
	workspacesDir := filepath.Join(tmp, "workspaces")
	cacheDir := filepath.Join(tmp, "cache")
	imagePath := filepath.Join(tmp, "image.qcow2")
	virshListPath := filepath.Join(tmp, "virsh-list.txt")
	virshDominfoPath := filepath.Join(tmp, "virsh-dominfo.txt")
	terraformPath := filepath.Join(binDir, "terraform")
	virshPath := filepath.Join(binDir, "virsh")

	for _, dir := range []string{binDir, workspacesDir, cacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(imagePath, []byte("image"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := os.WriteFile(virshListPath, []byte(" Id   Name   State\n--------------------\n"), 0o644); err != nil {
		t.Fatalf("write virsh list: %v", err)
	}
	if err := os.WriteFile(virshDominfoPath, []byte(""), 0o644); err != nil {
		t.Fatalf("write virsh dominfo: %v", err)
	}

	writeExecutable(t, terraformPath, "#!/bin/sh\necho \"terraform $*\"\n")
	writeExecutable(t, virshPath, "#!/bin/sh\ncase \"$1\" in\n  list)\n    cat \"$VIRSH_LIST_FILE\"\n    ;;\n  dominfo)\n    cat \"$VIRSH_DOMINFO_FILE\"\n    ;;\n  start)\n    if [ \"$VIRSH_FAIL\" = \"start\" ]; then\n      echo \"start failed\" >&2\n      exit 1\n    fi\n    echo \"Domain '$2' started\"\n    ;;\n  shutdown)\n    if [ \"$VIRSH_FAIL\" = \"shutdown\" ]; then\n      echo \"shutdown failed\" >&2\n      exit 1\n    fi\n    echo \"Domain '$2' shutdown initiated\"\n    ;;\n  *)\n    echo \"unsupported virsh command: $1\" >&2\n    exit 1\n    ;;\nesac\n")

	initGitRepo(t, repoDir)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("VIRSH_LIST_FILE", virshListPath)
	t.Setenv("VIRSH_DOMINFO_FILE", virshDominfoPath)

	r := runner.New(runner.Config{
		CoreRepoURL:        repoDir,
		TerraformBin:       terraformPath,
		WorkspacesDir:      workspacesDir,
		CloudImageCacheDir: cacheDir,
	})

	return &handlers{runner: r}, testEnv{
		workspacesDir:    workspacesDir,
		imagePath:        imagePath,
		virshListPath:    virshListPath,
		virshDominfoPath: virshDominfoPath,
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("test repo\n"), 0o644); err != nil {
		t.Fatalf("write repo file: %v", err)
	}

	runCommand(t, "", "git", "init", dir)
	runCommand(t, dir, "git", "add", "README.md")
	runCommand(t, dir, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "init")
}

func runCommand(t *testing.T, dir string, name string, args ...string) {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()

	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode JSON: %v\nbody: %s", err, rec.Body.String())
	}
}

func assertJSONContentType(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}
