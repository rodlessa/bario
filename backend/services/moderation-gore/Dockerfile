# syntax=docker/dockerfile:1
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    HF_HOME=/models

WORKDIR /app

# CPU-only torch keeps the image from pulling multi-gigabyte CUDA libraries.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt ./
RUN pip install --no-cache-dir \
        torch==2.15.0 torchvision==0.29.2 \
        --index-url https://download.pytorch.org/whl/cpu \
    && pip install --no-cache-dir -r requirements.txt

# Create the runtime user before the model download: chowning a populated
# /models afterwards would duplicate the weights in a new image layer.
RUN useradd --create-home --uid 10001 appuser \
    && mkdir -p /models \
    && chown appuser:appuser /models /app

COPY --chown=appuser:appuser scripts ./scripts
COPY --chown=appuser:appuser app ./app

USER appuser

# Only pytorch_model.bin and tokenizer files are fetched (see the script).
RUN python scripts/download_model.py

# Runtime is offline: weights and tokenizer files are baked into /models.
ENV TRANSFORMERS_OFFLINE=1 \
    HF_HUB_OFFLINE=1 \
    HTTP_PORT=8086 \
    METRICS_PORT=9096

EXPOSE 8086 9096

HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 \
    CMD python -c "import os, urllib.request; urllib.request.urlopen('http://127.0.0.1:' + os.environ.get('HTTP_PORT', '8086') + '/healthz', timeout=3)"

CMD ["python", "-m", "app.main"]
