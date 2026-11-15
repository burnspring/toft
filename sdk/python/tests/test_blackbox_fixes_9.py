"""Regressions for the 2026-11-04 black-box list's third round, Python's side
(BB-107, BB-108, BB-109, BB-113)."""

from __future__ import annotations

import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft.caller import ENV_ROLE_KEY, ROLE_HEADER  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun  # noqa: E402
from toft.viewer import _reset_identity_cache  # noqa: E402

from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402
from test_caller import KEY, mint  # noqa: E402

import time  # noqa: E402


def _refused(test, call, code):
    FakePlatform.requests = []
    with test.assertRaises(ToftError) as caught:
        call()
    err = caught.exception
    test.assertEqual(err.code, code, str(err))
    test.assertFalse(err.retryable, str(err))
    test.assertTrue(err.hint, f"{code} ({err}) has no hint")
    test.assertEqual(FakePlatform.requests, [], "a refusal was sent")
    return err


class TestAViewerDescribesThePerson(PlatformTestCase):
    """BB-107: with viewer identity on, repr names who() resolved — it said
    "the app cannot see who" beside it — and claims nothing before."""

    def setUp(self):
        super().setUp()
        _reset_identity_cache()

    def tearDown(self):
        _reset_identity_cache()
        super().tearDown()

    def test_repr_after_who(self):
        self.route("GET /api/v1/app/viewer", 200, {"id": "u1", "name": "Ada Lovelace", "role": "editor"})
        with mock.patch.dict(os.environ, {ENV_ROLE_KEY: KEY}):
            headers = {"X-Toft-Act-For": "blob-107", ROLE_HEADER: mint(KEY, "editor", int(time.time()) + 60)}
            v = toft.viewer(headers)
            self.assertNotIn("cannot see who", repr(v))
            self.assertIn("a signed-in person", repr(v))
            self.assertEqual(toft.who(headers).name, "Ada Lovelace")
            self.assertEqual(repr(v), "<toft.Viewer (Ada Lovelace, editor)>")


class TestTailRefusesAPollIntervalThatIsNotPositive(PlatformTestCase):
    """BB-108: tail(poll_interval=0) busy-looped into the rate limiter; it is
    bad_request at the call, before any request, as wait() has it."""

    def test_refused_at_the_call(self):
        run = JobRun.from_raw({"id": "r1"})
        for interval in (0, -1, float("nan"), True, "2"):
            _refused(self, lambda: run.tail(poll_interval=interval), "bad_request")


class TestAStoreNameThatIsNotAString(PlatformTestCase):
    """BB-109: download(None) was invalid_name "a name is required", and
    download(5) / remove(5) / url(5) were hinted with upload()."""

    def test_each_call_names_itself(self):
        with mock.patch.dict(os.environ, {"TOFT_STORAGE": '[{"name":"x","kind":"store","path":"","access":"full","folder":"y"}]'}):
            s = toft.store("x", folder="y")
            for call in ("download", "remove", "url", "link"):
                for name in (None, 5):
                    err = _refused(self, lambda: getattr(s, call)(name), "bad_request")
                    self.assertTrue(err.hint.startswith(f"pass the name as a string: {call}("), err.hint)
            err = _refused(self, lambda: s.list(5), "bad_request")
            self.assertIn("list(", err.hint)


class TestARedirectIsCodedWithRsHint(PlatformTestCase):
    """BB-113's sweep: Python coded it already; the hint is now R's, in all four."""

    def test_redirect(self):
        self.route("GET /api/v1/app/data", 302, {}, {"Location": "https://sign-in.example/"})
        with self.assertRaises(ToftError) as caught:
            toft.data.get()
        err = caught.exception
        self.assertEqual((err.code, err.status), ("unexpected_redirect", 302))
        self.assertIn("TOFT_API_URL should be the address Toft set for this app", err.hint)


if __name__ == "__main__":
    unittest.main()
