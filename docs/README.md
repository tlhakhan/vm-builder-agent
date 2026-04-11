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

# copy certs (generated in the mTLS Setup section below)
sudo mkdir -p /etc/vm-builder-agent/certs
sudo cp certs/vm-builder-agent.crt certs/vm-builder-agent.key certs/ca.crt /etc/vm-builder-agent/certs/

# create the workspaces directory
sudo mkdir -p /var/lib/vm-builder-agent/workspaces

# install and enable the service
sudo cp docs/examples/vm-builder-agent.service /etc/systemd/system/vm-builder-agent.service
sudo systemctl daemon-reload
sudo systemctl enable --now vm-builder-agent
```

Check status and logs:

```bash
systemctl status vm-builder-agent
journalctl -u vm-builder-agent -f
```

---

## Development Testing (no mTLS)

Good for local development and curl testing.

### Start the agent

```bash
mkdir -p ~/vm-builder-workspaces

./vm-builder-agent \
  --listen        :8080 \
  --core-repo     https://github.com/tlhakhan/vm-builder-core \
  --terraform     terraform \
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
curl -s -N -X POST http://localhost:8080/vm/create \
  -H 'Content-Type: application/json' \
  -d '{
    "name":                 "ubuntu-0",
    "cpu":                  4,
    "memoryGib":            8,
    "disksGib":             [64],
    "cloudImageUrl":        "file:///home/ubuntu/downloads/noble-server-cloudimg-amd64.img",
    "consoleUser":          "ubuntu",
    "consolePassword":      "ubuntu",
    "automationUser":       "ubuntu",
    "automationUserPubkey": "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDNCeFA0v8d3GQIHmMG1T/CmGieRnqE2Vhwno0qYwgi2PwODbv3sjUFDUHphxLzVjbPf7m7bSRkigVPynfsiV/Ml+Wbt3FgNygy2WGbOJKoSMhKPAwPZwfrpdG/OkO4q+SjBiqFuBYQVcJ47a4+jrj9C6Fbse3fR43/ZxxIZDKCMlIZBC75J2uXHSiwbVYIh/EX2w4unZgO8KyecuRawQCbB3I+LZBKpinnJn+Sb6vJB57GdDlEmVfqPp4nbSDq9xSbtrpoFkniiVxwTsdj9E2xCsfASJywrQeNWVDVnHvHDSxwxBWfmzsfHCJ/N+xDrU0WY6BBoof33ja1+4fhSEZ/LKoLD2H57z18xsgANUHPu1VgUfGn/Wp9BfJ7BYIfD57XufA4IGftFTIcZAGTEFRxEpHQWJ68s5SkN7lyrC3+ph/BJVCS+PnBe78Zjbi0SOdfpzvJQI7/xXVYfOUAVC+undyljKdpHUQpd6BsRmChBEKDnxlupdBVYzJb4mLKlu0=",
    "pciDevices":           []
  }'
```

The response streams terraform output line by line. The first line is the job ID:

```
job_id: 4a3f9c1d2e8b7f6a

=== cloning https://github.com/tlhakhan/vm-builder-core into ~/vm-builder-workspaces/ubuntu-0 ===
...
DONE
```

A duplicate create for `intel` while one is in flight, or after a successful create without a prior delete, returns `409 Conflict`.

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
curl -s -N -X DELETE http://localhost:8080/vm/ubuntu-0
```

Streams terraform destroy output. Workspace is removed on success.

### Get job status

Every create and delete returns a `job_id` on the first line of the stream.
Poll it after the fact:

```bash
curl -s http://localhost:8080/jobs/<job_id> | jq
```

### Health check

```bash
curl -s http://localhost:8080/health | jq
```

---

## mTLS Setup

The agent requires TLS 1.3 and validates the client certificate CN against
`vm-builder-apiserver` (configurable via `--client-cn`).

### 1. Create a directory for certificates

```bash
mkdir -p certs && cd certs
```

### 2. Generate the CA

```bash
openssl genrsa -out ca.key 4096

openssl req -new -x509 -days 3650 \
  -key ca.key \
  -out ca.crt \
  -subj "/CN=vm-builder-ca"
```

### 3. Generate the server certificate

The SAN must include `localhost` and `127.0.0.1` so curl trusts it.

```bash
openssl genrsa -out vm-builder-agent.key 4096

openssl req -new \
  -key vm-builder-agent.key \
  -out vm-builder-agent.csr \
  -subj "/CN=localhost"

openssl x509 -req -days 365 \
  -in vm-builder-agent.csr \
  -CA ca.crt -CAkey ca.key -CAcreateserial \
  -extfile <(printf "subjectAltName=DNS:localhost,IP:127.0.0.1") \
  -out vm-builder-agent.crt
```

### 4. Generate the client certificate

The CN must match `--client-cn` (default: `vm-builder-apiserver`).

```bash
openssl genrsa -out client.key 4096

openssl req -new \
  -key client.key \
  -out client.csr \
  -subj "/CN=vm-builder-apiserver"

openssl x509 -req -days 365 \
  -in client.csr \
  -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out client.crt
```

```bash
cd ..  # back to repo root
```

### 5. Start the agent with mTLS

```bash
mkdir -p ~/vm-builder-workspaces

./vm-builder-agent \
  --listen         :8443 \
  --mtls \
  --cert           certs/vm-builder-agent.crt \
  --key            certs/vm-builder-agent.key \
  --ca             certs/ca.crt \
  --core-repo      https://github.com/tlhakhan/vm-builder-core \
  --terraform      terraform \
  --workspaces-dir ~/vm-builder-workspaces
```

---

## Manual Tests — mTLS

Prefix every curl with the three TLS flags:

```bash
CURL_TLS="--cacert certs/ca.crt --cert certs/client.crt --key certs/client.key"
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
curl -s -N $CURL_TLS -X POST $BASE/vm/create \
  -H 'Content-Type: application/json' \
  -d '{
    "name":                 "ubuntu-0",
    "cpu":                  4,
    "memoryGib":            8,
    "disksGib":             [64],
    "cloudImageUrl":        "file:///home/ubuntu/downloads/noble-server-cloudimg-amd64.img",
    "consoleUser":          "ubuntu",
    "consolePassword":      "ubuntu",
    "automationUser":       "ubuntu",
    "automationUserPubkey": "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQDNCeFA0v8d3GQIHmMG1T/CmGieRnqE2Vhwno0qYwgi2PwODbv3sjUFDUHphxLzVjbPf7m7bSRkigVPynfsiV/Ml+Wbt3FgNygy2WGbOJKoSMhKPAwPZwfrpdG/OkO4q+SjBiqFuBYQVcJ47a4+jrj9C6Fbse3fR43/ZxxIZDKCMlIZBC75J2uXHSiwbVYIh/EX2w4unZgO8KyecuRawQCbB3I+LZBKpinnJn+Sb6vJB57GdDlEmVfqPp4nbSDq9xSbtrpoFkniiVxwTsdj9E2xCsfASJywrQeNWVDVnHvHDSxwxBWfmzsfHCJ/N+xDrU0WY6BBoof33ja1+4fhSEZ/LKoLD2H57z18xsgANUHPu1VgUfGn/Wp9BfJ7BYIfD57XufA4IGftFTIcZAGTEFRxEpHQWJ68s5SkN7lyrC3+ph/BJVCS+PnBe78Zjbi0SOdfpzvJQI7/xXVYfOUAVC+undyljKdpHUQpd6BsRmChBEKDnxlupdBVYzJb4mLKlu0=",
    "pciDevices":           []
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
curl -s -N $CURL_TLS -X DELETE $BASE/vm/ubuntu-0
```

### Verify CN enforcement

A request without a client certificate should be rejected:

```bash
curl -s --cacert certs/ca.crt https://localhost:8443/health
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

curl -s --cacert certs/ca.crt --cert /tmp/wrong.crt --key certs/client.key \
  https://localhost:8443/health
# expected: forbidden: unexpected client CN
```
