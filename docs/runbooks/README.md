# Bario runbooks

Operational notes for the Bario services and platform. These are written against
the current chart and source, not an idealized architecture.

## Common access

The live demo is exposed through the Traefik ingress on `vm1`:

```bash
# If bario.local is in /etc/hosts (recommended):
#   192.168.122.92  bario.local
curl -fsS http://bario.local/api/auth/healthz

# Without a hosts entry:
curl -fsS --resolve bario.local:80:192.168.122.92 http://bario.local/api/auth/healthz
```

Cluster:

```bash
ssh vm1@192.168.122.92
kubectl -n bario get pods -o wide
kubectl -n bario get events --sort-by=.lastTimestamp | tail -30
```

Grafana: `http://bario.local/grafana/` (user `admin`; password from the
`bario-grafana` Secret, key `admin-password`). The Prometheus datasource and
dashboards are provisioned from ConfigMaps.

```bash
kubectl -n bario get secret bario-grafana \
  -o jsonpath='{.data.admin-password}' | base64 -d
```

## Runbooks

- [auth](auth.md) — identity, JWT issuance, public profiles
- [posts](posts.md) — timeline, feed, ads interleaving, cursor pagination
- [follows](follows.md) — follower graph
- [uploads](uploads.md) — image ingest, moderation fan-out, quarantine/audit
- [ads](ads.md) — campaign CRUD, serving, impressions/clicks
- [moderation](moderation.md) — NSFWJS and CLIP gore sidecars
- [frontend](frontend.md) — Vue SPA, cache headers, stale-bundle incident
- [platform](platform.md) — Helm, registry, PostgreSQL, Prometheus/Grafana, rollouts

## Test logins

Demo seed users are created by `scripts/seed.sh`. Set `SEED_PASSWORD` in your
shell to the value passed to that script (the development default is defined in
`scripts/seed.sh`); the examples below use `$SEED_PASSWORD`. Use `ada_lovelace`
for feed tests.

## Before redeploying any service

```bash
cd ~/chirp
make test                     # go vet + frontend typecheck
make guard                    # deterministic secret scan of staged changes
sh scripts/secret_scan.sh --help
```

Then follow the per-service "Redeploy" section and verify the new pod start
time/image digest, not just the Helm exit code.
