# Bario — Orange Pi migration plan

Groundwork for moving off the single VM onto a cluster of Orange Pi boards.

## Current (VM)

- Single node `vm1` (x86_64, 8 CPU, 7.2 GiB, 38 GiB disk).
- k3s single-node, Traefik ingress, local-path storage.
- Local registry at `127.0.0.1:5000` (insecure HTTP, trusted by k3s via
  `/etc/rancher/k3s/registries.yaml`).

## Target (Orange Pi cluster)

- **1 master** — 4 GiB: k3s server (control plane, Traefik, metrics-server, and the
  "heavy" services below).
- **N workers** — 2 GiB each: k3s agents running the lightweight Bario services.
- **ARM64 (aarch64)** — every image must be multi-arch.

## Image multi-arch

All base images (golang:1.24-alpine, node:22, python:3.12-slim, postgres:16-alpine,
nginxinc/nginx-unprivileged) are already multi-arch. The Go services build static
binaries, so the only change is the build platform.

Build + push per service (or via `make` once buildx is wired in):

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t <registry>/bario/<svc>:latest --push .
```

`docker buildx` on the VM can emulate arm64 via QEMU; or build natively on one Orange
Pi and push to the registry.

## Resource budget (2 GiB workers)

| Service | Typical | Fits 2 GiB? | Placement |
|---|---|---|---|
| auth / posts / uploads / follows / ads | 32–64 MiB | yes | workers |
| frontend (nginx) | 32 MiB | yes | workers |
| moderation (NSFWJS/tfjs) | ~500 MiB | tight | master |
| moderation-gore (CLIP/torch) | 1–1.5 GiB | **no** | master |
| postgres | 256–512 MiB | tight | master |

So the split is: **heavy (moderation, moderation-gore, postgres) on the master**;
**light (Go services + frontend) on workers**. This is the key resource-tuning change
the Helm chart needs (node affinity / nodeSelector + tightened limits).

## Node placement (Helm changes needed)

- Taint the master (`node-role.kubernetes.io/control-plane`) and tolerate it for the
  heavy services, or use a `node-type: heavy`/`light` label + nodeSelector.
- Lower `moderation-gore.resources.limits.memory` from `3Gi` to something that fits
  a 4 GiB master (e.g. `2Gi`), and `moderation` from `2Gi` to `1Gi`.
- Keep the Go services at their current small requests.

## Multi-node k3s

```bash
# master (4 GiB)
curl -sfL https://get.k3s.io | sh -
# agent (2 GiB), token from /var/lib/rancher/k3s/server/node-token
curl -sfL https://get.k3s.io | K3S_URL=https://<master>:6443 K3S_TOKEN=<token> sh -
```

## Registry for multi-node

The current registry is VM-local (`127.0.0.1:5000`) — fine for one node, not reachable
from Orange Pi agents. For multi-node either:

1. Deploy the registry **inside k3s** (Deployment + ClusterIP Service) so every node
   reaches it via the cluster network, or
2. Run it on the master at a node-reachable address and point the agents'
   `registries.yaml` at that.

## Migration steps

1. Wire `docker buildx` multi-arch into the Makefile (`make docker-buildx`).
2. Build + push multi-arch images to the registry.
3. Provision Orange Pi boards (flash OS, install k3s server + agents).
4. Point k3s at the registry (registries.yaml on every node).
5. Add node placement + tightened limits to the Helm chart.
6. `helm upgrade` into the new cluster; migrate Postgres with `pg_dump`/`pg_restore`.
7. Re-point DNS/ingress to the master.
