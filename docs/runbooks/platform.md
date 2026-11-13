# platform runbook

Cluster-wide operations for the Bario demo on `vm1`.

## Topology

- k3s single node `vm1` (`192.168.122.92`), Traefik ingress, local-path storage
- Namespace `bario`
- Local registry `127.0.0.1:5000` on the VM; k3s is configured to trust it
- Ingress host `bario.local`; frontend at `/`, APIs under `/api/<service>`,
  Grafana under `/grafana`
- PostgreSQL 16 StatefulSet `bario-postgres-0`; app schemas: auth, posts,
  follows, uploads, ads
- Prometheus scrapes the Bario metrics services, node-exporter,
  kube-state-metrics, and kubelet cAdvisor; Grafana is provisioned from
  ConfigMaps

## Access

```bash
ssh vm1@192.168.122.92
kubectl -n bario get pods -o wide
kubectl -n bario get svc
kubectl -n bario get ingress
```

For browser access from another machine, add:

```text
192.168.122.92  bario.local
```

or use `curl --resolve bario.local:80:192.168.122.92 ...`.

## Deploy / redeploy

The chart is `deploy/bario`. The demo release uses:

```bash
helm upgrade --install bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 \
  --set ingress.className=traefik
```

Current image policy: app images use `:latest` with `pullPolicy: Always`, and
every app pod template carries `bario.dev/revision: {{ .Release.Revision }}`.
A Helm upgrade therefore rolls the app pods and pulls the current image even
when only the image bytes changed. Pinned images (Grafana, Prometheus,
node-exporter, kube-state-metrics, PostgreSQL) can stay on `IfNotPresent`.

After deploying, verify the actual pod, not just Helm:

```bash
kubectl -n bario get pods -o wide
kubectl -n bario get pods -l app.kubernetes.io/component=posts \
  -o jsonpath='{range .items[*]}{.metadata.name}{" start="}{.status.startTime}{" image="}{.status.containerStatuses[0].imageID}{"\n"}{end}'
```

### Push an image to the VM registry

Build on the dev machine (amd64 node):

```bash
cd ~/chirp
docker build -f backend/services/posts/Dockerfile \
  -t 127.0.0.1:5000/bario/posts:latest backend
```

Expose the VM-local registry through SSH while pushing:

```bash
SOCK=/tmp/vm1-registry.sock
ssh -M -S "$SOCK" -f -N -o ExitOnForwardFailure=yes \
  -L 5000:127.0.0.1:5000 vm1@192.168.122.92
docker push 127.0.0.1:5000/bario/posts:latest
ssh -S "$SOCK" -O exit vm1@192.168.122.92
```

Do not use `k3s ctr images import` for persistent images; a k3s restart clears
the imported images. The registry is the bridge between Docker and k3s.

## Stale-image trap (real incidents)

Both the posts and frontend were once served from pre-change images after a
rebuild: the Deployment used `:latest` + `IfNotPresent`, so a rebuild alone did
not create a new pod or pull the new image. We confirmed that `Always` alone is
also insufficient when the Deployment spec is unchanged.

The fix is the combination now in the chart:

1. app `pullPolicy: Always`
2. `bario.dev/revision` annotation from `.Release.Revision` on app pod
   templates, so every `helm upgrade` rolls the pods.

If a change is not live, check:

```bash
kubectl -n bario get deploy bario-posts \
  -o jsonpath='{.spec.template.spec.containers[0].imagePullPolicy}{" "}{.spec.template.metadata.annotations}{"\n"}'
kubectl -n bario get pods -l app.kubernetes.io/component=posts \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.startTime}{" "}{.status.containerStatuses[0].imageID}{"\n"}{end}'
```

Then confirm the registry digest actually changed and re-run the Helm upgrade.
Do not manually patch live Deployment specs; chart upgrades overwrite them.

## PostgreSQL: uid 70 + AppArmor saga

PostgreSQL 16 had two distinct startup problems on this k3s/Ubuntu host:

1. **`su-exec postgres` hang** — the entrypoint waited before initdb. Running
   the pod as uid/gid 70 (the postgres user) makes the entrypoint skip the
   privilege-drop step. The chart sets pod-level `runAsUser/runAsGroup/fsGroup:
   70`.
2. **`could not signal for checkpoint: Permission denied`** — AppArmor signal
   mediation blocked `kill()` syscalls, including a process signaling itself
   (confirmed with strace). The chart sets container-level
   `appArmorProfile.type: Unconfined` for PostgreSQL.

Health:

```bash
kubectl -n bario get pod bario-postgres-0
kubectl -n bario logs bario-postgres-0 --tail=80
kubectl -n bario exec bario-postgres-0 -- \
  pg_isready -U bario -d bario

kubectl -n bario exec bario-postgres-0 -- \
  psql -U bario -d bario -c '\dt *.*'
```

If PostgreSQL is healthy, `database system is ready to accept connections`
should be in the logs. If initdb never starts, re-check the pod securityContext
(uid 70) and container AppArmor profile. Do not remove `fsGroup: 70`; the PVC
needs to be writable by the postgres user.

## Seed / reset test data

Seed data is API-driven:

```bash
cd ~/chirp
AUTH_URL=http://bario.local/api/auth \
POSTS_URL=http://bario.local/api/posts \
FOLLOWS_URL=http://bario.local/api/follows \
scripts/seed.sh
```

The script is safe to rerun. If the database was recreated, users will be
missing until seeding. The demo login is `ada_lovelace` with the seed password passed to
`scripts/seed.sh` (development only). Inspect data directly only for diagnosis:

```bash
kubectl -n bario exec bario-postgres-0 -- \
  psql -U bario -d bario -c 'select count(*) from auth.users;'
```

## Prometheus / Grafana

```bash
kubectl -n bario get pods -l app.kubernetes.io/component=prometheus
kubectl -n bario get pods -l app.kubernetes.io/component=grafana
kubectl -n bario get pods -l app.kubernetes.io/component=node-exporter
kubectl -n bario get pods -l app.kubernetes.io/component=kube-state-metrics
```

Check scrape targets without exposing Prometheus:

```bash
POD=$(kubectl -n bario get pods -l app.kubernetes.io/component=prometheus \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n bario exec "$POD" -- \
  wget -qO- http://127.0.0.1:9090/api/v1/targets \
  | jq -r '.data.activeTargets[].labels.job' | sort -u
```

Expected jobs: bario-auth, bario-posts, bario-uploads, bario-follows, bario-ads,
bario-moderation, bario-moderation-gore, node-exporter, kube-state-metrics,
kubelet-cadvisor. If a Bario job is present in the ConfigMap but absent from
targets, the Prometheus pod has not reloaded its mounted config. The chart now
adds a checksum annotation to the Prometheus pod template, so a Helm upgrade
rolls it when `prometheus-config.yaml` changes. Verify the new pod start time.

Grafana is at `http://bario.local/grafana/`; password:

```bash
kubectl -n bario get secret bario-grafana \
  -o jsonpath='{.data.admin-password}' | base64 -d
```

Dashboards `Bario — Platform Overview`, `Bario — Service Detail`, and
`Bario — Kubernetes` are provisioned from `deploy/bario/dashboards` via a
ConfigMap; do not recreate them in the UI. Grafana's own state is on an
ephemeral emptyDir, so users/annotations do not survive pod replacement, but
datasource/dashboards always re-provision.

## Frontend caching

The frontend nginx config sends `Cache-Control: no-cache` for `index.html` and
SPA fallback routes, and `public, immutable` with a one-year expiry for
Vite-hashed `/assets/*`. If a browser shows an old app shell:

```bash
curl -sSI --resolve bario.local:80:192.168.122.92 http://bario.local/ \
  | grep -i cache-control
# expect: Cache-Control: no-cache
```

Hard-reload once; hashed assets should remain cached.

## Common diagnostics

```bash
kubectl -n bario get events --sort-by=.lastTimestamp | tail -40
kubectl -n bario describe pod <pod>
kubectl -n bario logs <pod> --previous --tail=100
kubectl -n bario get deploy,sts,ds,pods
helm -n bario status bario
helm -n bario history bario
```

Do not commit secrets. Generated values live in Kubernetes Secrets:
`bario-postgres` (`password`), `bario-jwt` (`jwt-secret`), and `bario-grafana`
(`admin-password`). The secret scanner is:

```bash
make guard
# or: sh scripts/secret_scan.sh --staged
```
