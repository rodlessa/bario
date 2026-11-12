# Bario — Session Wrap-up & Next Steps

Status as of wrap-up (Thu Nov 12 2026).

## Current state

**Code** — repo `~/chirp`, master @ `1ffac78`, working tree clean.
Full Bario app: 5 Go services (auth, posts, uploads, follows, ads) + Vue 3 frontend
(classic/modern toggle) + moderation sidecars (NSFWJS, CLIP gore) + Helm chart + TDD
suites for all 4 core services. Deterministic secret scanner (`scripts/secret_scan.sh`).

**VM** — `vm1@192.168.122.92` (SSH `~/.ssh/id_ed25519`, passwordless sudo), Ubuntu 26.04.
- k3s v1.37.2 + Traefik + local-path storage.
- Local registry `127.0.0.1:5000` with all 8 bario images pushed; trusted via
  `/etc/rancher/k3s/registries.yaml`.
- Running 1/1: frontend, moderation, moderation-gore, prometheus.
- postgres + Go services: still converging (postgres fix in flight — see below).

## Postgres: root causes found (two separate issues)

1. **`su-exec postgres` hangs** → FIXED. Postgres runs as uid 70
   (`securityContext: runAsUser/runAsGroup/fsGroup 70`) so the entrypoint skips
   `su-exec`. Commit `1ffac78`.
2. **`could not signal for checkpoint: Permission denied`** → root cause **AppArmor
   signal mediation**. `strace` showed `kill()` → **EACCES** even for self-signaling
   (`kill(1, SIGURG)`), which only an LSM can cause. Ruled out along the way: stale
   data, seccomp (an `Unconfined` seccomp test does NOT touch AppArmor), hidepid,
   image/libc variant (alpine and debian both fail). **Fix: `securityContext.
   appArmorProfile: {type: Unconfined}` on the postgres container.** Being finalized.

## Next steps (in order)

1. **Finalize the AppArmor fix** — Coder adds `appArmorProfile: {type: Unconfined}` to
   the postgres pod (values-driven, backward-compatible); Ledger commits; redeploy.
   Confirm postgres reaches `database system is ready to accept connections`.
2. **Scale up + verify** — scale auth/posts/uploads/follows/ads back to 1; confirm all
   Ready; test timeline (login `ada_lovelace` / `bario-seed-dev`, seed data exists).
3. **Feed integration — KNOWN GAP (plan 3e)** — `posts` has NO `ads/serve` wiring: the
   ads service is built and deployed, but `GET /feed` never calls it. Coder must wire
   posts → `ads/serve` (interleave `kind=ad` items, 5%-scroll-scaled ratio) + frontend
   rendering. This is a real product gap, not just a Helm gap.
4. **Jenkins + private GitHub repo** — GitHub user `rodlessar`. Jenkins placement
   (VM vs cluster) still unconfirmed. This is the E2E/build-pipeline work.
5. **Orange Pi migration** — `MIGRATION.md` has the plan (multi-arch buildx, resource
   tuning for 2GB workers, multi-node k3s). Groundwork already committed
   (`global.registry`, node placement, tightened limits, `make docker-buildx`).

## How to resume

- **Maestri workflow**: Architect (plans/verifies, no code) · Coder (code) · Ledger
  (git) · Atlas/PM (coordination) · Nano (deterministic guardrail, escalation-only).
- **VM**: `ssh vm1@192.168.122.92` (sudo NOPASSWD).
- **Deploy**: `helm upgrade --install bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 --set ingress.className=traefik`.
- **Registry**: `127.0.0.1:5000` (8 images). Multi-arch rebuild: `make docker-buildx`.

## Key lessons / gotchas

- Agents' cwd is `~/.hermes` but the repo is `~/chirp` → file-tool reads trip "read
  outside working directories" prompts (shell commands are fine). Consider moving the
  repo into the workspace or setting the agents' working directory.
- **Do NOT restart k3s after `k3s ctr images import`** — it clears the imported images.
  Use the registry (pull), not import, for anything persistent.
- **postgres + k3s + AppArmor**: needs `appArmorProfile: Unconfined`; AppArmor's signal
  mediation blocks postgres checkpoint signaling.
- **Docker (Ubuntu) and k3s use separate containerd stores** — `docker` images ≠ k3s
  images; the registry is the bridge.
