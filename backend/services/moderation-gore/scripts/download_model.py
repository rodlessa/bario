"""Download the CLIP model and processor into the Hugging Face cache.

Only the PyTorch checkpoint and tokenizer/preprocessor files are fetched, so
the image does not carry a duplicate safetensors copy. Used by the Docker
build; the runtime then starts with TRANSFORMERS_OFFLINE=1.
"""

import os

from huggingface_hub import snapshot_download
from transformers import CLIPModel, CLIPProcessor

MODEL = os.environ.get("CLIP_MODEL", "openai/clip-vit-base-patch32").strip()
MODEL = MODEL or "openai/clip-vit-base-patch32"

print(f"downloading {MODEL} into {os.environ.get('HF_HOME', 'default cache')}")
model_dir = snapshot_download(
    repo_id=MODEL,
    allow_patterns=[
        "config.json",
        "preprocessor_config.json",
        "tokenizer.json",
        "tokenizer_config.json",
        "vocab.json",
        "merges.txt",
        "special_tokens_map.json",
        "pytorch_model.bin",
    ],
)
print(f"snapshot at {model_dir}")
CLIPModel.from_pretrained(model_dir, use_safetensors=False)
CLIPProcessor.from_pretrained(model_dir)
print("model and processor ready")
