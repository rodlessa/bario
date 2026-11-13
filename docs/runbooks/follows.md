# follows runbook

## What it does

`follows` owns schema `follows` and the directed follower graph. It supports
idempotent follow/unfollow and lists followers/following, resolving display
names through auth.

- API: `:8080`, routes `/users/{id}/follow`, `/users/{id}/followers`,
  `/users/{id}/following` and the same under `/api/follows`
- Metrics: `:9090`, service label `follows`
- Database: `follows.follows(follower_id, followee_id, created_at)`, primary key
  `(follower_id, followee_id)`, index on `followee_id`
- Outbound: auth `GET /users/{id}` for usernames; best-effort

## Health check

```bash
curl -fsS --resolve bario.local:80:192.168.122.92 \
  http://bario.local/api/follows/healthz

kubectl -n bario get pods -l app.kubernetes.io/component=follows
kubectl -n bario logs deploy/bario-follows --tail=50
```

List check (public):

```bash
curl -fsS --resolve bario.local:80:192.168.122.92 \
  http://bario.local/api/follows/users/2/following | jq
```

Follow/unfollow check (auth required):

```bash
TOKEN=$(curl -fsS --resolve bario.local:80:192.168.122.92 \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"ada_lovelace\",\"password\":\"${SEED_PASSWORD}\"}" \
  http://bario.local/api/auth/login | jq -r .token)
curl -i -X POST --resolve bario.local:80:192.168.122.92 \
  -H "Authorization: Bearer $TOKEN" \
  http://bario.local/api/follows/users/3/follow
```

## Failure modes and response

| Symptom | Cause | Action |
| --- | --- | --- |
| posts feed returns `502 could not load the accounts you follow` | follows service unavailable or DB error in `FollowingIDs` | This is load-bearing for posts. Check `bario-follows`, its DB connection, and logs. |
| follower/following list names show `user-<id>` | auth username lookup failed or auth is down | Expected best-effort degradation; list still returns 200. Check auth. |
| follow returns `400 you cannot follow yourself` | self-target | Correct behavior. |
| follow returns 201 on repeated requests | `ON CONFLICT DO NOTHING` makes follow idempotent | Expected. |
| unfollow returns 204 when no edge existed | delete affects zero rows but still succeeds | Expected. |
| `500 could not follow/unfollow/load users` | DB error | Check PostgreSQL and logs. |
| list unexpectedly large | no pagination by design; lists return every relation ordered by `created_at DESC, id DESC` | Limit at the API consumer if needed; do not assume a page size. |
| `400 invalid id` | non-numeric or ≤0 path id | Use a valid user id. |

## Redeploy

```bash
cd ~/chirp
docker build -f backend/services/follows/Dockerfile \
  -t 127.0.0.1:5000/bario/follows:latest backend
# push to the VM registry (platform.md)
helm upgrade bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 \
  --set ingress.className=traefik
```

Confirm the new pod, then check follows health, a list call, and the posts feed
(which depends on follows).

## Tests

```bash
cd backend && go test ./services/follows/... -count=1
```
