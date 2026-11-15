"""toft.app.url(path): the shared join vector, and how the address is asked for.

The vector (sdk/testdata/app-url-vector.json) is read by all four SDKs. The rest
is Python's half of the fetch rules: asked of ``/api/v1/app/url``, reused for a
short while and then asked again (the address moves without a deploy), the last
answer used when the platform cannot be reached, and a refusal raised.
"""

from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft import appinfo  # noqa: E402
from toft.appinfo import join_app_url  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402

VECTOR = Path(__file__).resolve().parent.parent.parent / "testdata" / "app-url-vector.json"


class TestTheSharedVector(unittest.TestCase):
    def test_every_case(self):
        for case in json.loads(VECTOR.read_text())["cases"]:
            with self.subTest(case["name"]):
                if "error" in case:
                    with self.assertRaises(ToftError) as ctx:
                        join_app_url(case["public_url"], case["base_path"], case["path"])
                    self.assertEqual(ctx.exception.code, case["error"])
                    self.assertTrue(ctx.exception.hint)
                else:
                    self.assertEqual(join_app_url(case["public_url"], case["base_path"], case["path"]), case["want"])


class TestTheAddressIsAsked(PlatformTestCase):
    def answer(self, public_url, base_path="/apps/reporter"):
        self.route("GET /api/v1/app/url", 200, {"public_url": public_url, "base_path": base_path})

    def asked(self):
        return sum(1 for r in FakePlatform.requests if r["path"] == "/api/v1/app/url")

    def test_built_from_the_platforms_answer(self):
        self.answer("https://apps.example.com/apps/reporter")
        self.assertEqual(toft.app.url("/reports/42"), "https://apps.example.com/apps/reporter/reports/42")
        self.assertEqual(self.asked(), 1)

    def test_reused_briefly_then_asked_again(self):
        clock = [1000.0]
        with mock.patch.object(appinfo.time, "monotonic", lambda: clock[0]):
            self.answer("https://apps.example.com/apps/reporter")
            toft.app.url()
            toft.app.url("/a")
            self.assertEqual(self.asked(), 1)
            # The administrator moved the install; within the window the old
            # answer stands, after it the new one is used.
            self.answer("https://applications.example.org/apps/reporter")
            clock[0] += appinfo.URL_TTL_SECONDS + 1
            self.assertEqual(toft.app.url("/a"), "https://applications.example.org/apps/reporter/a")
            self.assertEqual(self.asked(), 2)

    def test_unreachable_uses_the_last_answer(self):
        clock = [1000.0]
        with mock.patch.object(appinfo.time, "monotonic", lambda: clock[0]):
            self.answer("https://apps.example.com/apps/reporter")
            toft.app.url()
            clock[0] += appinfo.URL_TTL_SECONDS + 1
            with mock.patch("toft.appinfo.transport") as t:
                t.return_value.get.side_effect = ToftError("connection refused")
                self.assertEqual(toft.app.url("/a"), "https://apps.example.com/apps/reporter/a")

    def test_unreachable_with_no_answer_raises(self):
        with mock.patch("toft.appinfo.transport") as t:
            t.return_value.get.side_effect = ToftError("connection refused")
            with self.assertRaises(ToftError):
                toft.app.url()

    def test_no_address_is_the_platforms_refusal(self):
        self.route("GET /api/v1/app/url", 503,
                   {"error": "no address", "code": "no_public_address", "retryable": False,
                    "hint": "an administrator sets APPS_BASE_URL"})
        with self.assertRaises(ToftError) as ctx:
            toft.app.url()
        self.assertEqual(ctx.exception.code, "no_public_address")
        self.assertTrue(ctx.exception.hint)


if __name__ == "__main__":
    unittest.main()
