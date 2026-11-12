# Gore moderation sidecar (CLIP)

Standalone, stateless zero-shot classifier for graphic violence and gore. It
runs OpenAI CLIP ViT-B/32 through PyTorch and returns softmax probabilities
over a fixed prompt set.

This is a **Python sidecar**, separate from the Go services and from
`backend/services/moderation/` (the NSFWJS one). CLIP is a PyTorch model, so
Python is the natural runtime; it has no database, no persistence, and is not
wired into uploads yet.

- **Runtime:** Python 3.12, PyTorch CPU, Hugging Face `transformers`
- **Model:** `openai/clip-vit-base-patch32` (ViT-B/32), about 151M parameters
  and roughly 600 MB of fp32 weights. Chosen over ViT-L/14 because it is much
  smaller and faster on CPU while remaining usable for zero-shot screening.
- **HTTP port:** `8086` (`HTTP_PORT`)
- **Metrics port:** `9096` (`METRICS_PORT`), same metric names as the other
  services with `service="moderation-gore"`
- **Max image size:** 10 MiB
- **Formats:** JPEG, PNG, GIF, BMP, WebP (Pillow); identical to the uploads
  service and the NSFWJS sidecar

## How classification works

The fixed prompt set is:

```text
graphic violence
gore
blood
a normal photo
everyday scene
```

CLIP embeds the image and all prompts, computes cosine-similarity logits,
scales them by CLIP's learned logit scale, and applies a softmax across the
five prompts. The response probabilities therefore sum to approximately 1 and
are comparable within one request. They are zero-shot scores, not calibrated
medical or forensic judgements.

## API

`GET /healthz` (also `/api/moderation-gore/healthz`) returns
`{"status":"ok"}`.

`POST /classify` (also `/api/moderation-gore/classify`) accepts:

- **multipart/form-data** with a file field named `file`:

  ```bash
  curl -F file=@test.jpg http://localhost:8086/classify
  ```

- **raw image bytes** with an `image/*` Content-Type:

  ```bash
  curl --data-binary @test.jpg \
    -H 'Content-Type: image/jpeg' \
    http://localhost:8086/classify
  ```

Example response from a real landscape photo:

```json
{"graphic violence":0.02907644584774971,"gore":0.13455741107463837,"blood":0.004527794197201729,"a normal photo":0.7514578104019165,"everyday scene":0.08038055151700974}
```

Errors use the `{"error":"..."}` shape:

| Status | Cause |
| ------ | ----- |
| `400`  | missing file, non-image body, unsupported format, or decode failure |
| `405`  | wrong method for the path |
| `413`  | body or image larger than 10 MiB |
| `500`  | model inference failed (the process stays up) |

## Metrics

`GET http://localhost:9096/metrics` exposes the canonical Bario metrics:

```text
bario_http_requests_total{endpoint="classify",method="POST",service="moderation-gore",status="200"} 1
bario_http_request_duration_seconds_bucket{endpoint="classify",le="0.1",method="POST",service="moderation-gore"} 1
```

The metrics listener is separate from the API listener, matching the
`HTTP_PORT` / `METRICS_PORT` split used by the Go services. Kubernetes and
Helm wiring are intentionally out of scope for this scaffold.

## Run locally

Python 3.12 is recommended, matching the Docker image.

```bash
cd backend/services/moderation-gore

python3.12 -m venv .venv
. .venv/bin/activate

# CPU-only PyTorch avoids pulling CUDA libraries.
pip install torch==2.15.0 torchvision==0.29.2 \
  --index-url https://download.pytorch.org/whl/cpu
pip install -r requirements.txt

# Optional: pre-download the model. The first start downloads it too.
python scripts/download_model.py

python -m app.main
```

The model loads before the service starts listening. On first run it downloads
about 600 MB of weights into the Hugging Face cache; set `HF_HOME` to choose
the cache location. Override `HTTP_PORT`, `METRICS_PORT`, and `CLIP_MODEL` if
needed.

Run the unit tests without loading the model:

```bash
python -m unittest discover -s tests
```

## Docker

```bash
docker build -t bario/moderation-gore:latest backend/services/moderation-gore
docker run --rm -p 8086:8086 -p 9096:9096 bario/moderation-gore:latest
```

The image installs CPU-only PyTorch and downloads the model during the build,
then sets `TRANSFORMERS_OFFLINE=1` / `HF_HUB_OFFLINE=1`, so runtime needs no
network. Expect an image around 2.8 GB: the CPU PyTorch runtime is most of it,
plus roughly 600 MB of CLIP weights.

## Status

This sidecar is standalone. Wiring it into the uploads service (and deciding
how its scores combine with the NSFWJS decision) is a follow-up task.
