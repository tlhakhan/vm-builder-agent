# vm-builder-agent — Manual Testing Guide

## Prerequisites

- A KVM/libvirt hypervisor — see [github.com/tlhakhan/hypervisors](https://github.com/tlhakhan/hypervisors) for a recommended setup
- `virsh` available (for VM list / detail / power operations)
- `openssl` for TLS certificate generation

---

## Build

```bash
go build -o vm-builder-agent .
```

---

## systemd Service

A ready-to-use unit file is provided at [`docs/examples/vm-builder-agent.service`](examples/vm-builder-agent.service).

Install it on the hypervisor:

```bash
# copy the binary
sudo cp vm-builder-agent /usr/local/bin/vm-builder-agent

# install and enable the service
sudo cp docs/examples/vm-builder-agent.service /etc/systemd/system/vm-builder-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now vm-builder-agent
```

> The service's `ExecStartPre` directives create all required directories
> (`workspaces`, `cloud-image-cache`, `private`) on first start.

Check status and logs:

```bash
systemctl status vm-builder-agent
journalctl -u vm-builder-agent -f
```

---

## Flag Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | `:8443` | Address to listen on |
| `--agent-mtls` | `false` | Enable mTLS for the agent listener |
| `--agent-trusted-ca-url` | _(required when mTLS enabled)_ | URL to the CA certificate used to verify client certificates |
| `--agent-authorized-client-cn` | `vm-builder-apiserver` | Expected client certificate CN for mTLS |
| `--private-dir` | `/etc/vm-builder-agent/private` | Directory where the agent stores its auto-generated TLS certificate and key |
| `--core-repo` | _(required)_ | Git URL for vm-builder-core |
| `--terraform` | `tofu` | Terraform/OpenTofu binary name or path |
| `--workspaces-dir` | `/var/lib/vm-builder-agent/workspaces` | Directory where per-VM Terraform workspaces are kept |
| `--cloud-image-cache-dir` | `/var/lib/vm-builder-agent/cloud-image-cache` | Directory where cloud images are cached to avoid repeated downloads |

---

## Development Testing (no mTLS)

Good for local development and curl testing.

### Start the agent

```bash
mkdir -p ~/vm-builder-workspaces

./vm-builder-agent \
  --listen        :8080 \
  --core-repo     https://github.com/tlhakhan/vm-builder-core \
  --terraform     tofu \
  --workspaces-dir ~/vm-builder-workspaces
```

> The agent logs JSON to stdout.

---

## Manual Tests — Development

All examples below target `http://localhost:8080`.

### Get node info

```bash
curl -s http://localhost:8080/node | jq
```

### List VMs

```bash
curl -s http://localhost:8080/vm | jq
```

### Create a VM

Cloud images can be downloaded from [https://cloud-images.ubuntu.com](https://cloud-images.ubuntu.com).

```bash
curl -s -X POST http://localhost:8080/vm/create \
  -H 'Content-Type: application/json' \
  -d '{
    "name":                   "ubuntu-0",
    "cpu":                    4,
    "memory_gib":             8,
    "disks_gib":              [64],
    "cloud_image_url":        "file:///home/ubuntu/downloads/noble-server-cloudimg-amd64.img",
    "console_user":           "ubuntu",
    "console_password":       "ubuntu",
    "automation_user":        "ubuntu",
    "automation_user_pubkey": "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDNCeFA0v8d3GQIHmMG1T/CmGieRnqE2Vhwno0qYwgi2PwODbv3sjUFDUHphxLzVjbPf7m7bSRkigVPynfsiV/Ml+Wbt3FgNygy2WGbOJKoSMhKPAwPZwfrpdG/OkO4q+SjBiqFuBYQVcJ47a4+jrj9C6Fbse3fR43/ZxxIZDKCMlIZBC75J2uXHSiwbVYIh/EX2w4unZgO8KyecuRawQCbB3I+LZBKpinnJn+Sb6vJB57GdDlEmVfqPp4nbSDq9xSbtrpoFkniiVxwTsdj9E2xCsfASJywrQeNWVDVnHvHDSxwxBWfmzsfHCJ/N+xDrU0WY6BBoof33ja1+4fhSEZ/LKoLD2H57z18xsgANUHPu1VgUfGn/Wp9BfJ7BYIfD57XufA4IGftFTIcZAGTEFRxEpHQWJ68s5SkN7lyrC3+ph/BJVCS+PnBe78Zjbi0SOdfpzvJQI7/xXVYfOUAVC+undyljKdpHUQpd6BsRmChBEKDnxlupdBVYzJb4mLKlu0=",
    "pci_devices":            ["0000:01:00.0", "0000:01:00.1"]
  }'
```

The request blocks until terraform completes and returns JSON with the combined command output:

```json
{"name":"ubuntu-0","output":"...terraform output..."}
```

Creating a VM that already exists returns `409 Conflict`.

### Get VM details

```bash
curl -s http://localhost:8080/vm/ubuntu-0 | jq
```

Returns virsh `dominfo` merged with the creation parameters from the workspace.

### Power off a VM (graceful)

```bash
curl -s -X POST http://localhost:8080/vm/ubuntu-0/shutdown | jq
```

### Power on a VM

```bash
curl -s -X POST http://localhost:8080/vm/ubuntu-0/start | jq
```

### Delete a VM

```bash
curl -s -X DELETE http://localhost:8080/vm/ubuntu-0 | jq
```

Blocks until terraform destroy completes. Returns JSON with command output. Workspace is removed on success.

### Health check

```bash
curl -s http://localhost:8080/health | jq
```

---

## mTLS Setup

The agent requires TLS 1.3 and validates the client certificate CN against
`vm-builder-apiserver` (configurable via `--agent-authorized-client-cn`).

The client-trust CA is now provided with `--agent-trusted-ca-url`. The agent fetches that CA
certificate at startup and uses it to verify incoming client certificates.

The agent no longer requires a manually created server certificate. When mTLS is
enabled it creates a self-signed certificate/key pair in `--private-dir`
(default: `/etc/vm-builder-agent/private`) and reuses it on later starts.

### 1. Generate the client CA and client certificate

Only the CA and client certificate need to be created by hand now. The client
certificate CN must match `--agent-authorized-client-cn` (default: `vm-builder-apiserver`).

```bash
mkdir -p certs && cd certs

openssl genrsa -out ca.key 4096

openssl req -new -x509 -days 3650 \
  -key ca.key \
  -out ca.crt \
  -subj "/CN=vm-builder-ca"

openssl genrsa -out client.key 4096

openssl req -new \
  -key client.key \
  -out client.csr \
  -subj "/CN=vm-builder-apiserver"

openssl x509 -req -days 365 \
  -in client.csr \
  -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out client.crt

cd ..  # back to repo root
```

### 2. Publish the CA certificate somewhere the agent can fetch it

Any reachable `http://` or `https://` URL works. For local testing you can serve
the cert directly:

```bash
cd certs
python3 -m http.server 9000
```

With that running, the CA URL becomes `http://localhost:9000/ca.crt`.

### 3. Start the agent with mTLS

```bash
mkdir -p ~/vm-builder-workspaces
mkdir -p /tmp/vm-builder-agent-private

./vm-builder-agent \
  --listen         :8443 \
  --agent-mtls \
  --agent-trusted-ca-url http://localhost:9000/ca.crt \
  --private-dir    /tmp/vm-builder-agent-private \
  --core-repo      https://github.com/tlhakhan/vm-builder-core \
  --terraform      tofu \
  --workspaces-dir ~/vm-builder-workspaces
```

The agent will create these files on first start:

```text
/tmp/vm-builder-agent-private/vm-builder-agent.crt
/tmp/vm-builder-agent-private/vm-builder-agent.key
```

---

## Manual Tests — mTLS

Prefix every curl with the agent's generated server certificate plus the client
certificate pair:

```bash
CURL_TLS="--cacert /tmp/vm-builder-agent-private/vm-builder-agent.crt --cert certs/client.crt --key certs/client.key"
BASE=https://localhost:8443
```

### Get node info

```bash
curl -s $CURL_TLS $BASE/node | jq
```

### List VMs

```bash
curl -s $CURL_TLS $BASE/vm | jq
```

### Create a VM

Cloud images can be downloaded from [https://cloud-images.ubuntu.com](https://cloud-images.ubuntu.com).

```bash
curl -s $CURL_TLS -X POST $BASE/vm/create \
  -H 'Content-Type: application/json' \
  -d '{
    "name":                   "ubuntu-0",
    "cpu":                    4,
    "memory_gib":             8,
    "disks_gib":              [64],
    "cloud_image_url":        "file:///home/ubuntu/downloads/noble-server-cloudimg-amd64.img",
    "console_user":           "ubuntu",
    "console_password":       "ubuntu",
    "automation_user":        "ubuntu",
    "automation_user_pubkey": "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDNCeFA0v8d3GQIHmMG1T/CmGieRnqE2Vhwno0qYwgi2PwODbv3sjUFDUHphxLzVjbPf7m7bSRkigVPynfsiV/Ml+Wbt3FgNygy2WGbOJKoSMhKPAwPZwfrpdG/OkO4q+SjBiqFuBYQVcJ47a4+jrj9C6Fbse3fR43/ZxxIZDKCMlIZBC75J2uXHSiwbVYIh/EX2w4unZgO8KyecuRawQCbB3I+LZBKpinnJn+Sb6vJB57GdDlEmVfqPp4nbSDq9xSbtrpoFkniiVxwTsdj9E2xCsfASJywrQeNWVDVnHvHDSxwxBWfmzsfHCJ/N+xDrU0WY6BBoof33ja1+4fhSEZ/LKoLD2H57z18xsgANUHPu1VgUfGn/Wp9BfJ7BYIfD57XufA4IGftFTIcZAGTEFRxEpHQWJ68s5SkN7lyrC3+ph/BJVCS+PnBe78Zjbi0SOdfpzvJQI7/xXVYfOUAVC+undyljKdpHUQpd6BsRmChBEKDnxlupdBVYzJb4mLKlu0=",
    "pci_devices":            ["0000:01:00.0", "0000:01:00.1"]
  }'
```

### Get VM details

```bash
curl -s $CURL_TLS $BASE/vm/ubuntu-0 | jq
```

### Power off a VM

```bash
curl -s -X POST $CURL_TLS $BASE/vm/ubuntu-0/shutdown | jq
```

### Power on a VM

```bash
curl -s -X POST $CURL_TLS $BASE/vm/ubuntu-0/start | jq
```

### Delete a VM

```bash
curl -s -X DELETE $CURL_TLS $BASE/vm/ubuntu-0 | jq
```

### Verify CN enforcement

A request without a client certificate should be rejected:

```bash
curl -s --cacert /tmp/vm-builder-agent-private/vm-builder-agent.crt https://localhost:8443/health
# expected: curl: (56) ... certificate required
```

A request with a client cert whose CN is not `vm-builder-apiserver` should receive
`403 Forbidden`:

```bash
# generate a wrong-cn cert
openssl req -new -key certs/client.key -out /tmp/wrong.csr -subj "/CN=attacker"
openssl x509 -req -days 1 -in /tmp/wrong.csr \
  -CA certs/ca.crt -CAkey certs/ca.key -CAcreateserial \
  -out /tmp/wrong.crt

curl -s --cacert /tmp/vm-builder-agent-private/vm-builder-agent.crt --cert /tmp/wrong.crt --key certs/client.key \
  https://localhost:8443/health
# expected: forbidden: unexpected client CN
```
