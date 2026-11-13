# auth runbook

## What it does

`auth` is the identity service. It owns schema `auth` and the `auth.users`
table, registers accounts, verifies bcrypt passwords, issues 24-hour JWTs, and
serves public profiles. It has no outbound HTTP clients.

- API: `:8080`, routes `/register`, `/login`, `/users/{id}` and the same under
  `/api/auth`
- Metrics: `:9090`, service label `auth`
- Database: `auth.users(id, username UNIQUE, password_hash, email, is_admin,
  interests TEXT[], created_at)`
- JWT signed with the shared `JWT_SECRET`; every service validates it locally
- `ADMIN_USERNAME` (default `admin`) is marked admin at registration time

## Health check

```bash
curl -fsS --resolve bario.local:80:192.168.122.92 \
  http://bario.local/api/auth/healthz
# {"status":"ok"}

kubectl -n bario get pods -l app.kubernetes.io/component=auth
kubectl -n bario logs deploy/bario-auth --tail=50
```

Metrics:

```bash
kubectl -n bario port-forward svc/bario-auth-metrics 9090:9090
curl -s http://127.0.0.1:9090/metrics | grep bario_http
```

## Login smoke test

```bash
TOKEN=$(curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"ada_lovelace\",\"password\":\"${SEED_PASSWORD}\"}" \
  http://bario.local/api/auth/login | jq -r .token)
curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H "Authorization: Bearer $TOKEN" http://bario.local/api/auth/users/2
```

## Failure modes and response

| Symptom | Cause | Action |
| --- | --- | --- |
| `/healthz` 503 / pod not Ready | DB connection or migration failure | Check `kubectl logs deploy/bario-auth`; check PostgreSQL with `pg_isready`; confirm `DATABASE_URL` Secret wiring. |
| login returns `401 invalid username or password` | Wrong password, unknown user, or an empty/reseeded database | Check the user exists in `auth.users`; re-run `scripts/seed.sh` if this is a fresh database. Use the seed password passed to `scripts/seed.sh` (the development default is defined there). |
| registration returns `409 username is already taken` | Case-sensitive unique username collision | Choose another username or inspect `auth.users`. |
| registration returns `400` | Username not matching `^[A-Za-z0-9_]{3,32}$`, password outside 8–72 chars, invalid email, >32 interests, or a >64-char tag | Fix the request; the error body names the invalid field. |
| `500 could not sign in/create account/load user` | DB error, bcrypt failure, or JWT signing error | Check pod logs and database health. A missing/short `JWT_SECRET` stops the service at startup. |
| All services reject tokens | `JWT_SECRET` changed or Secret mismatch | Roll auth and the dependent services from the same release/Secret. |
| Existing admin user is no longer admin | `ADMIN_USERNAME` changed after registration | Admin is set only at registration; update the row deliberately with SQL if intended. |

Do not log passwords or JWTs. The login response contains a real bearer token;
treat command output as sensitive.

## Redeploy

```bash
cd ~/chirp
docker build -f backend/services/auth/Dockerfile \
  -t 127.0.0.1:5000/bario/auth:latest backend
# push to the VM registry (see platform.md for the SSH tunnel command)
helm upgrade bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 \
  --set ingress.className=traefik
```

The chart uses `pullPolicy: Always` plus a per-release `bario.dev/revision`
annotation, so the upgrade rolls the pod and pulls the new `:latest` image.
Confirm the pod's `startTime` and `imageID`, then repeat the health/login checks.

## Tests

```bash
cd backend && go test ./services/auth/... -count=1
```
