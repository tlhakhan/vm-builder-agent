# vm-builder-agent

A lightweight HTTP agent that runs on KVM/libvirt hypervisors and exposes a small API for VM lifecycle management. It is the hypervisor-side component of a two-tier homelab VM management system.

## Motivation

Managing VMs across multiple homelab hypervisors by SSH-ing into each one and running terraform by hand gets old fast. The goal of this project is a clean API surface that sits on each hypervisor, accepts requests from a central controller, and handles the full VM lifecycle — provisioning, power management, and teardown — without any manual intervention on the hypervisor itself.

The agent is intentionally small. It does not try to be a full orchestration system. It delegates all provisioning logic to [vm-builder-core](https://github.com/tlhakhan/vm-builder-core) (a terraform + libvirt module) and exposes the results over HTTP. The central controller ([vm-builder-apiserver](https://github.com/tlhakhan/vm-builder-apiserver), built separately) talks to one agent per hypervisor over mTLS.

## Architecture

```
vm-builder-apiserver  (Raspberry Pi — built separately)
      ↕  mTLS
vm-builder-agent  (this repo — one instance per hypervisor)
      ↕  local
vm-builder-core  (terraform + libvirt provisioning)
```

## What it does

- Clones [vm-builder-core](https://github.com/tlhakhan/vm-builder-core) into a per-VM workspace on each create request
- Writes a `terraform.tfvars` from the request payload and runs `terraform init + apply`
- Blocks until the operation completes and returns the result with command output
- Caches cloud images locally so subsequent creates skip re-downloading
- Persists the terraform workspace (and state file) so destroy works later
- Exposes virsh-backed endpoints for VM listing, details, and power management
- Reports hypervisor node info (CPU, memory, disk, running VMs, PCI passthrough devices)
- Discovers vfio-pci bound devices and reports availability (free vs attached to a running VM)
- Enforces one operation per VM at a time — duplicate requests get a `409 Conflict`
- Supports toggleable mTLS with client CN verification, a CA loaded from URL, and an auto-generated self-signed server certificate

## API

| Method   | Path                  | Description                              |
|----------|-----------------------|------------------------------------------|
| `POST`   | `/vm/create`          | Provision a new VM (synchronous)         |
| `DELETE` | `/vm/:name`           | Destroy a VM (synchronous)               |
| `GET`    | `/vm`                 | List all VMs via virsh                   |
| `GET`    | `/vm/:name`           | VM details + creation params             |
| `POST`   | `/vm/:name/start`     | Power on a VM                            |
| `POST`   | `/vm/:name/shutdown`  | Graceful shutdown                        |
| `GET`    | `/node`               | Hypervisor node info                     |
| `GET`    | `/health`             | Uptime                                   |

All API responses are JSON with `Content-Type: application/json`. Error responses use:

```json
{"error":"message"}
```

Create and delete block until terraform completes and return the command output:

```json
{"name":"ubuntu-0","output":"...terraform output..."}
```

## Related

- [vm-builder-core](https://github.com/tlhakhan/vm-builder-core) — terraform + libvirt provisioning module
- [hypervisors](https://github.com/tlhakhan/hypervisors) — recommended hypervisor setup

## Quick start

Build and run in plain HTTP mode for local dev testing:

```bash
go build -o vm-builder-agent .

mkdir -p ~/vm-builder-workspaces

./vm-builder-agent \
  --listen         :8080 \
  --core-repo      https://github.com/tlhakhan/vm-builder-core \
  --terraform      tofu \
  --workspaces-dir ~/vm-builder-workspaces
```

```bash
curl -s http://localhost:8080/health | jq
```

For a full walkthrough of all endpoints and mTLS certificate setup see [docs/README.md](docs/README.md).
