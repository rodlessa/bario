import unittest
from io import BytesIO

from PIL import Image

from app.images import DecodeImageError, UnsupportedImage, decode_image, detect_image_format


def padded(prefix: bytes) -> bytes:
    return prefix + b"\x00" * (32 - len(prefix))


class ImageSniffingTests(unittest.TestCase):
    def test_detects_accepted_formats(self):
        self.assertEqual(detect_image_format(padded(b"\xff\xd8\xff\xe0")), "jpeg")
        self.assertEqual(detect_image_format(padded(b"\x89PNG\r\n\x1a\n")), "png")
        self.assertEqual(detect_image_format(padded(b"GIF89a")), "gif")
        self.assertEqual(detect_image_format(padded(b"BM")), "bmp")
        self.assertEqual(detect_image_format(padded(b"RIFF\x00\x00\x00\x00WEBP")), "webp")

    def test_rejects_unknown_and_ico(self):
        self.assertIsNone(detect_image_format(b"not an image"))
        self.assertIsNone(detect_image_format(padded(b"\x00\x00\x01\x00")))
        with self.assertRaises(UnsupportedImage):
            decode_image(padded(b"\x00\x00\x01\x00"))

    def test_decodes_real_png(self):
        buffer = BytesIO()
        Image.new("RGB", (8, 8), (10, 20, 30)).save(buffer, format="PNG")
        image = decode_image(buffer.getvalue())
        self.assertEqual(image.mode, "RGB")
        self.assertEqual(image.size, (8, 8))

    def test_rejects_corrupt_supported_signature(self):
        with self.assertRaises(DecodeImageError):
            decode_image(padded(b"\xff\xd8\xff\xe0"))


if __name__ == "__main__":
    unittest.main()
