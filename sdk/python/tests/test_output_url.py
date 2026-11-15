"""write_output mints its URL at the moment it writes and falls back to
TOFT_OUTPUT_URI; every output PUT carries Azure Blob's x-ms-blob-type
(security review 2026-11-14, AS-2 and AS-4)."""

from __future__ import annotations

import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from toft.client import Run  # noqa: E402

from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402


def _header(request, name):
    return {k.lower(): v for k, v in request["headers"].items()}.get(name)


class TestWriteOutputMintsItsURL(PlatformTestCase):
    def _run(self):
        with mock.patch.dict(os.environ, {"TOFT_OUTPUT_URI": self.base + "/stale"}):
            return Run(self.base, "run-token", "r1")

    def test_the_minted_url_is_used(self):
        run = self._run()
        self.route("POST /api/v1/runner/output-url", 200, {"url": self.base + "/fresh"})
        self.route("PUT /fresh", 201, {})
        run.write_output(b"x")
        put = self.last()
        self.assertEqual(put["path"], "/fresh")
        self.assertEqual(_header(put, "x-ms-blob-type"), "BlockBlob")
        mint = FakePlatform.requests[-2]
        self.assertEqual(mint["path"], "/api/v1/runner/output-url")
        self.assertEqual(mint["headers"].get("Authorization"), "Bearer run-token")

    def test_toft_output_uri_is_the_fallback(self):
        run = self._run()
        self.route("POST /api/v1/runner/output-url", 404, {"error": "not found"})
        self.route("PUT /stale", 201, {})
        run.write_output(b"x")
        put = self.last()
        self.assertEqual(put["path"], "/stale")
        self.assertEqual(_header(put, "x-ms-blob-type"), "BlockBlob")


if __name__ == "__main__":
    unittest.main()
