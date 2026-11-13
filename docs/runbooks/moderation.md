# moderation sidecars runbook

Two stateless sidecars screen uploaded images before uploads stores them. They
share a format contract but use different runtimes and models.

## moderation (NSFWJS, Node)

- HTTP `:8085`, metrics `:9095`, service label `moderation`
- `POST /classify` (also `/api/moderation/classify`), raw `image/*` or
  multipart field `file`, max 10 MiB
- `GET /healthz` (also `/api/moderation/healthz`)
- NSFWJS 4.4.0 MobileNetV2 on TensorFlow.js; weights bundled in the image
- No database, no persistence
- WebP is converted to raw sRGB with `sharp` before classification because
  tfjs cannot decode WebP
- Formats: JPEG, PNG, GIF, BMP, WebP; ICO/TIFF/AVIF are not accepted
- Responses: `200` five class probabilities; `400` bad/unsupported/decode;
  `413` oversize; `500` inference failure. Errors use `{"error":"..."}`.

## moderation-gore (CLIP, Python)

- HTTP `:8086`, metrics `:9096`, service label `moderation-gore`
- `POST /classify` (also `/api/moderation-gore/classify`), same body contract
- `GET /healthz` (also `/api/moderation-gore/healthz`)
- CLIP `openai/clip-vit-base-patch32` (ViT-B/32) on CPU PyTorch; ~600 MB fp32
- Fixed prompts: `graphic violence`, `gore`, `blood`, `a normal photo`,
  `everyday scene`
- Pillow decodes; `Image.MAX_IMAGE_PIXELS = 64_000_000` guards decompression
  bombs; no database, no persistence
- Formats identical to the NSFWJS sidecar; ICO/TIFF/AVIF rejected
- Responses: `200` five prompt probabilities; `400`, `413`, `500` as above.

## Health checks

```bash
kubectl -n bario get pods -l app.kubernetes.io/component=moderation
kubectl -n bario get pods -l app.kubernetes.io/component=moderation-gore
kubectl -n bario logs deploy/bario-moderation --tail=80
kubectl -n bario logs deploy/bario-moderation-gore --tail=80

kubectl -n bario port-forward svc/bario-moderation 8085:8085 &
curl -fsS http://127.0.0.1:8085/healthz
kill %1

kubectl -n bario port-forward svc/bario-moderation-gore 8086:8086 &
curl -fsS http://127.0.0.1:8086/healthz
kill %1
```

Classify with a real image:

```bash
kubectl -n bario port-forward svc/bario-moderation 8085:8085 &
curl -fsS -H 'Content-Type: image/png' --data-binary @test.png \
  http://127.0.0.1:8085/classify
kill %1

kubectl -n bario port-forward svc/bario-moderation-gore 8086:8086 &
curl -fsS -H 'Content-Type: image/png' --data-binary @test.png \
  http://127.0.0.1:8086/classify
kill %1
```

Metrics (both expose the canonical counter/histogram):

```bash
kubectl -n bario port-forward svc/bario-moderation-metrics 9095:9095 &
curl -s http://127.0.0.1:9095/metrics | grep 'endpoint="classify"'
kill %1

kubectl -n bario port-forward svc/bario-moderation-gore-metrics 9096:9096 &
curl -s http://127.0.0.1:9096/metrics | grep 'endpoint="classify"'
kill %1
```

Counters are in-memory and reset whenever the pod restarts; absence of a
classify series does not necessarily mean instrumentation is missing. Generate
one `/classify` request first.

## Failure modes and response

| Symptom | Cause | Action |
| --- | --- | --- |
| uploads returns 503 `image moderation is unavailable` | one/both sidecars error or timeout; uploads default `MODERATION_FAIL_OPEN=false` | Check both sidecars' `/healthz`, logs, and latency. The uploads call has a 5s timeout. Treat fail-open as a policy decision, not a fix. |
| uploads accepts with `moderation_status=unavailable` | fail-open enabled and a sidecar failed | Review why the sidecar failed; the image skipped classification. |
| `/classify` 413 | image/body >10 MiB | Compress image; uploads also enforces 10 MiB. |
| `/classify` 400 unsupported format | ICO/TIFF/AVIF or undecodable bytes | Use JPEG/PNG/GIF/BMP/WebP; keep the format allowlists aligned. |
| NSFWJS 400 `could not decode image` for WebP | sharp/tfjs decode issue | Confirm the WebP is valid; the sidecar's WebP path uses sharp, not tfjs. |
| CLIP `/classify` 500 | model/inference failure | Check pod memory/CPU; the model is ~600 MB and runs on CPU. A 2 GiB limit is tight but expected to fit. |
| Pod OOMKilled / CrashLoopBackOff on gore | CLIP model + PyTorch exceeds memory | Check `kubectl describe pod`; restore the 2 GiB limit or reduce concurrency/model size deliberately. |
| Slow classify (~seconds) | CPU CLIP/tfjs inference | Expected on CPU; uploads calls sidecars in parallel and times out at 5s. |
| Metrics `endpoint="classify"` missing | no classify traffic since pod start, or stale Prometheus scrape config | Send a real classify request first; then check Prometheus targets include bario-moderation/bario-moderation-gore. |

## Important scoring caveat

CLIP is a **zero-shot** classifier, not a calibrated forensic detector.
Probabilities are comparable only within one request and can be confidently
wrong on out-of-distribution or noisy images. The gore score is the sum of the
three explicit prompts, with a default uploads threshold of 0.7. Investigate
borderline rejects via `uploads.moderation_audit` scores; do not treat a CLIP
score as a medical or forensic judgement.

## Format contract (real project history)

The three format lists — uploads, NSFWJS `images.js`, and CLIP `images.py` —
must stay identical:

- JPEG, PNG, GIF, BMP, WebP are accepted.
- WebP is special-cased in NSFWJS (`sharp`); CLIP uses Pillow.
- ICO can sniff as `image/x-icon` in Go but neither tfjs nor sharp decodes it,
  so uploads rejects it before any classify call.
- TIFF/AVIF are also rejected at uploads.

Any change to one allowlist must change all three and include a classify test.

## Redeploy

```bash
cd ~/chirp
docker build -f backend/services/moderation/Dockerfile \
  -t 127.0.0.1:5000/bario/moderation:latest backend
docker build -f backend/services/moderation-gore/Dockerfile \
  -t 127.0.0.1:5000/bario/moderation-gore:latest backend
# push both to the VM registry (platform.md)
helm upgrade bario deploy/bario -n bario \
  --set global.registry=127.0.0.1:5000 \
  --set ingress.className=traefik
```

Confirm new pod start times, then classify a known image against both. The app
pods roll every upgrade via `bario.dev/revision`, so in-memory metrics start
fresh.

## Tests

```bash
cd backend/services/moderation && npm test
cd ../moderation-gore && python -m unittest discover -s tests -v
```
