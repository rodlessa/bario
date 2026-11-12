# Moderation sidecar

Standalone, stateless image-classification service for Bario. It runs the
[NSFWJS](https://github.com/infinitered/nsfwjs) MobileNetV2 model on
TensorFlow.js and returns the five NSFWJS class probabilities.

This is a **Node sidecar**, separate from the Go services. It has no database,
no persistence, and is called by the uploads service for every image before the
file is stored.

- **Runtime:** Node.js 22 LTS (Docker image), `@tensorflow/tfjs-node`, `sharp`
- **Model:** NSFWJS 4.4.0 MobileNetV2; the weights ship inside the `nsfwjs`
  npm package, so nothing is downloaded at runtime
- **HTTP port:** `8085` (`HTTP_PORT`)
- **Metrics port:** `9095` (`METRICS_PORT`), same metric names as the Go services
- **Max image size:** 10 MiB

## Run locally

```bash
cd backend/services/moderation
npm ci
npm start
```

The service loads the model before listening; startup takes a second or two.
Node 22 is recommended. The server includes a small compatibility shim for
newer Node releases where `util.isNullOrUndefined` was removed.

> npm 12 blocks dependency install scripts by default. This package lists
> `@tensorflow/tfjs-node` under `allowScripts`, so `npm ci` should run them. If
> your npm still blocks them, run `npm install-scripts approve @tensorflow/tfjs-node`
> and reinstall. npm 10/11 run the scripts normally.

## API

`GET /healthz` (also `/api/moderation/healthz`) returns `{"status":"ok"}`.

`POST /classify` (also `/api/moderation/classify`) accepts either:

- **multipart/form-data** with a file field named `file`:

  ```bash
  curl -F file=@test.jpg http://localhost:8085/classify
  ```

- **raw image bytes** with an `image/*` Content-Type:

  ```bash
  curl --data-binary @test.jpg \
    -H 'Content-Type: image/jpeg' \
    http://localhost:8085/classify
  ```

Supported formats: JPEG, PNG, GIF, BMP, and WebP. TensorFlow.js decodes the
first four directly; WebP is normalised to raw sRGB pixels with
[`sharp`](https://sharp.pixelplumbing.com/) and then wrapped in the same
integer RGB tensor before classification. The accepted set matches the uploads
service exactly: ICO (which Go sniffs as `image/x-icon`), TIFF, and AVIF are
rejected by both sides. The response is JSON in stable class order:

```json
{"Neutral":0.7892746925354004,"Drawing":0.20774567127227783,"Sexy":0.00000427293070970336,"Hentai":0.002954118885099888,"Porn":0.000021283674868755043}
```

The five probabilities sum to approximately 1. Errors use the same
`{"error":"..."}` shape as the Go services:

| Status | Cause |
| ------ | ----- |
| `400`  | missing file, non-image body, unsupported format, or decode failure |
| `405`  | wrong method for the path |
| `413`  | image larger than 10 MiB |
| `500`  | model inference failed (the process stays up) |

## Metrics

`GET http://localhost:9095/metrics` exposes the canonical Bario metrics for
`service="moderation"`, with endpoints `classify` and `health`:

```
bario_http_requests_total{service="moderation",endpoint="classify",method="POST",status="200"} 4
bario_http_request_duration_seconds_bucket{le="0.05",service="moderation",endpoint="classify",method="POST"} 2
```

The metrics listener is separate from the API listener, matching the Go
services' `HTTP_PORT` / `METRICS_PORT` split. A Kubernetes metrics Service and
Helm wiring are intentionally out of scope for this scaffold; when added, the
deployment should set `HTTP_PORT=8080` and `METRICS_PORT=9090` and expose the
metrics port only through a `ClusterIP` Service.

## Test

```bash
npm test
```

Unit tests cover image sniffing and response formatting; they do not load the
model. For an end-to-end check, start the service and POST any JPEG, PNG, or
WebP as shown above.

## Docker

```bash
docker build -t bario/moderation:latest backend/services/moderation
docker run --rm -p 8085:8085 -p 9095:9095 bario/moderation:latest
```

The image uses `node:22-bookworm-slim`, installs production dependencies only,
and bundles the model through the `nsfwjs` package. `tfjs-node` fetches its
native `libtensorflow` runtime during the build, so the build needs network;
runtime does not. `sharp` ships its own prebuilt libvips, so it needs no extra
system packages. Expect roughly 1.3 GB uncompressed because of the native
TensorFlow libraries (the model weights themselves are only a few MB).
