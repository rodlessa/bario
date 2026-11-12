"""CLIP zero-shot classifier for graphic violence and gore.

This is a standalone sidecar: no database, no persistence, and no uploads
wiring yet. It mirrors the NSFWJS sidecar's HTTP/metrics conventions.
"""

import logging
import os
import time
from contextlib import asynccontextmanager

import torch
import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from prometheus_client import (
    CollectorRegistry,
    Counter,
    Histogram,
    disable_created_metrics,
    start_http_server,
)
from starlette.concurrency import run_in_threadpool
from starlette.exceptions import HTTPException as StarletteHTTPException
from transformers import CLIPModel, CLIPProcessor

from .images import DecodeImageError, UnsupportedImage, decode_image

SERVICE = "moderation-gore"
PROMPTS = (
    "graphic violence",
    "gore",
    "blood",
    "a normal photo",
    "everyday scene",
)
MAX_BODY_BYTES = 10 * 1024 * 1024
DEFAULT_MODEL = "openai/clip-vit-base-patch32"

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
logger = logging.getLogger(SERVICE)


def env_port(name: str, fallback: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return fallback
    value = int(raw)
    if value < 1 or value > 65535:
        raise ValueError(f"{name} must be a TCP port between 1 and 65535")
    return value


try:
    HTTP_PORT = env_port("HTTP_PORT", 8086)
    METRICS_PORT = env_port("METRICS_PORT", 9096)
except ValueError as exc:
    logger.error("invalid configuration: %s", exc)
    raise SystemExit(2) from exc

MODEL_NAME = os.environ.get("CLIP_MODEL", DEFAULT_MODEL).strip() or DEFAULT_MODEL

registry = CollectorRegistry()
# Match the Go services: expose only the counter/histogram series, not the
# Python client's extra *_created timestamp series.
disable_created_metrics()
requests_total = Counter(
    "bario_http_requests_total",
    "Total number of HTTP requests handled by a Bario service.",
    ["service", "endpoint", "method", "status"],
    registry=registry,
)
request_duration = Histogram(
    "bario_http_request_duration_seconds",
    "HTTP request latency in seconds.",
    ["service", "endpoint", "method"],
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
    registry=registry,
)

model: CLIPModel | None = None
processor: CLIPProcessor | None = None


class APIError(Exception):
    def __init__(self, status_code: int, message: str):
        super().__init__(message)
        self.status_code = status_code
        self.message = message


def endpoint_label(path: str, method: str) -> str | None:
    if method == "GET" and path in ("/healthz", "/api/moderation-gore/healthz"):
        return "health"
    if method == "POST" and path in ("/classify", "/api/moderation-gore/classify"):
        return "classify"
    return None


def classify_image(image) -> list[float]:
    if model is None or processor is None:
        raise RuntimeError("model is not loaded")

    inputs = processor(text=list(PROMPTS), images=image, return_tensors="pt", padding=True)
    with torch.inference_mode():
        outputs = model(**inputs)
    return outputs.logits_per_image.softmax(dim=1)[0].tolist()


def content_length(request: Request) -> int | None:
    raw = request.headers.get("content-length")
    if not raw:
        return None
    try:
        return int(raw)
    except ValueError:
        return None


async def read_image_bytes(request: Request) -> bytes:
    content_type = request.headers.get("content-type", "").split(";")[0].strip().lower()

    if content_type == "multipart/form-data":
        length = content_length(request)
        if length is not None and length > MAX_BODY_BYTES + (1 << 20):
            raise APIError(413, "image exceeds the 10 MiB limit")
        try:
            form = await request.form(max_part_size=MAX_BODY_BYTES)
        except Exception as exc:
            raise APIError(400, "invalid multipart form") from exc
        upload = form.get("file")
        if upload is None or not hasattr(upload, "read"):
            raise APIError(400, 'multipart field "file" is required')
        data = await upload.read()
        await upload.close()
    elif content_type.startswith("image/"):
        length = content_length(request)
        if length is not None and length > MAX_BODY_BYTES:
            raise APIError(413, "image exceeds the 10 MiB limit")
        data = await request.body()
    else:
        raise APIError(400, 'expected multipart/form-data field "file" or an image Content-Type')

    if not data:
        raise APIError(400, "image is required")
    if len(data) > MAX_BODY_BYTES:
        raise APIError(413, "image exceeds the 10 MiB limit")
    return data


@asynccontextmanager
async def lifespan(_app: FastAPI):
    global model, processor

    threads = max(1, min(4, os.cpu_count() or 1))
    torch.set_num_threads(threads)
    started = time.perf_counter()
    logger.info("loading CLIP model %s", MODEL_NAME)
    model = CLIPModel.from_pretrained(MODEL_NAME, use_safetensors=False).eval()
    processor = CLIPProcessor.from_pretrained(MODEL_NAME)
    logger.info("CLIP model loaded in %.2fs", time.perf_counter() - started)

    start_http_server(METRICS_PORT, addr="0.0.0.0", registry=registry)
    logger.info("metrics listening on :%d", METRICS_PORT)
    yield
    logger.info("shutting down")


app = FastAPI(title="Bario gore moderation sidecar", lifespan=lifespan)


@app.middleware("http")
async def instrument(request: Request, call_next):
    endpoint = endpoint_label(request.url.path, request.method)
    started = time.perf_counter()
    try:
        response = await call_next(request)
    except Exception:
        if endpoint is not None:
            requests_total.labels(SERVICE, endpoint, request.method, "500").inc()
            request_duration.labels(SERVICE, endpoint, request.method).observe(
                time.perf_counter() - started
            )
        raise

    if endpoint is not None:
        requests_total.labels(SERVICE, endpoint, request.method, str(response.status_code)).inc()
        request_duration.labels(SERVICE, endpoint, request.method).observe(
            time.perf_counter() - started
        )
    return response


@app.exception_handler(APIError)
async def api_error_handler(_request: Request, exc: APIError):
    return JSONResponse(status_code=exc.status_code, content={"error": exc.message})


@app.exception_handler(StarletteHTTPException)
async def http_error_handler(_request: Request, exc: StarletteHTTPException):
    if exc.status_code == 404:
        message = "not found"
    elif exc.status_code == 405:
        message = "method not allowed"
    else:
        message = str(exc.detail)
    return JSONResponse(
        status_code=exc.status_code,
        content={"error": message},
        headers=exc.headers,
    )


@app.get("/healthz")
@app.get("/api/moderation-gore/healthz")
async def healthz():
    return {"status": "ok"}


@app.post("/classify")
@app.post("/api/moderation-gore/classify")
async def classify(request: Request):
    data = await read_image_bytes(request)

    try:
        image = await run_in_threadpool(decode_image, data)
    except UnsupportedImage as exc:
        raise APIError(
            400,
            "unsupported image format (JPEG, PNG, GIF, BMP or WebP required)",
        ) from exc
    except DecodeImageError as exc:
        raise APIError(400, "could not decode image") from exc

    try:
        probabilities = await run_in_threadpool(classify_image, image)
    except Exception as exc:
        logger.exception("classification failed")
        raise APIError(500, "classification failed") from exc

    return {prompt: float(probability) for prompt, probability in zip(PROMPTS, probabilities)}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=HTTP_PORT, log_level="info")
