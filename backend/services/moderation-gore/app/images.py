"""Image sniffing and decoding for the gore-moderation sidecar."""

from io import BytesIO

from PIL import Image, ImageOps, UnidentifiedImageError

# Refuse decompression bombs before Pillow allocates an enormous pixel buffer.
Image.MAX_IMAGE_PIXELS = 64_000_000


class UnsupportedImage(Exception):
    """The bytes are not one of the formats this sidecar accepts."""


class DecodeImageError(Exception):
    """The bytes matched a supported signature but Pillow could not decode them."""


def detect_image_format(data: bytes) -> str | None:
    """Return the accepted format name, or None.

    Keep this list identical to backend/services/moderation/src/images.js and
    the uploads service: JPEG, PNG, GIF, BMP and WebP.
    """
    if not isinstance(data, (bytes, bytearray)) or len(data) < 12:
        return None

    if data[0:3] == b"\xff\xd8\xff":
        return "jpeg"
    if data[0:8] == b"\x89PNG\r\n\x1a\n":
        return "png"
    if data[0:6] in (b"GIF87a", b"GIF89a"):
        return "gif"
    if data[0:2] == b"BM":
        return "bmp"
    if data[0:4] == b"RIFF" and data[8:12] == b"WEBP":
        return "webp"
    return None


def decode_image(data: bytes) -> Image.Image:
    """Decode bytes into an RGB PIL image suitable for CLIP."""
    if detect_image_format(data) is None:
        raise UnsupportedImage("unsupported image format")

    try:
        with Image.open(BytesIO(data)) as image:
            image.load()
            image = ImageOps.exif_transpose(image)
            return image.convert("RGB")
    except (UnidentifiedImageError, OSError, ValueError, Image.DecompressionBombError) as exc:
        raise DecodeImageError("could not decode image") from exc
