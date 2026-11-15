"""Regressions for the 2026-11-04 black-box list's second half, Python's side
(BB-008, BB-009, BB-013, BB-023, BB-025)."""

from __future__ import annotations

import inspect
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft.client import Run  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun  # noqa: E402

from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402

_PLATFORM_VARS = ("TOFT_API_URL", "TOFT_APP_ID", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR", "TOFT_STORAGE")
_URI = "https://bucket.s3.amazonaws.com/out?X-Amz-Credential=AKIAXYZ&X-Amz-Security-Token=SESSIONTOK"
_TOKEN = "job-secret-token-bb013"


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


class TestSDKMadeRefusalsAreCoded(PlatformTestCase):
    """BB-008: a refusal the SDK makes itself is coded, never a TypeError."""

    def test_notify_status_refuses_a_keyword(self):
        _refused(self, lambda: toft.notify.status("x", bogus=1), "bad_request")
        _refused(self, lambda: toft.notify.status(None), "bad_request")
        _refused(self, lambda: toft.notify.status(5), "bad_request")

    def test_store_names_and_sources(self):
        with tempfile.TemporaryDirectory() as d:
            src = Path(d, "a.txt")
            src.write_text("a")
            with mock.patch.dict(os.environ, {"TOFT_STORAGE":
                    f'[{{"name":"st","kind":"store","path":"{d}","access":"full"}}]'}):
                s = toft.store("st")
                _refused(self, lambda: s.upload(src, 5), "bad_request")
                _refused(self, lambda: s.upload({}), "bad_request")
                _refused(self, lambda: s.put(src, {}), "bad_request")
                _refused(self, lambda: s.put(None), "bad_request")
                _refused(self, lambda: s.download(5), "bad_request")
                _refused(self, lambda: s.url(5), "bad_request")
                _refused(self, lambda: s.remove(5), "bad_request")
                _refused(self, lambda: s.list(5), "bad_request")

    def test_update_attempts(self):
        for wrong in (0, -1, "3", 1.5, True):
            _refused(self, lambda: toft.data.update(lambda d: d, attempts=wrong), "bad_request")


class TestWaitTimeouts(unittest.TestCase):
    """BB-009: a timeout that is not one is bad_request, not a retryable
    wait_timeout — a finished run included."""

    def test_bad_timeouts(self):
        for status in ("running", "succeeded"):
            run = JobRun.from_raw({"id": "r1", "status": status})
            for wrong in (-1, float("nan"), float("inf"), "10"):
                _refused(self, lambda: run.wait(timeout=wrong), "bad_request")
            _refused(self, lambda: run.wait(timeout=1, poll_interval=0), "bad_request")

    def test_zero_is_a_timeout(self):
        run = JobRun.from_raw({"id": "r1", "status": "succeeded"})
        self.assertIs(run.wait(timeout=0), run)


class TestLogsCursor(unittest.TestCase):
    """BB-025: a negative or non-integer cursor is refused, as Node refuses it."""

    def test_bad_cursors(self):
        run = JobRun.from_raw({"id": "r1", "status": "running"})
        for wrong in (-1, "3", 1.5, None, True):
            _refused(self, lambda: run.logs(wrong), "bad_request")


class TestRunHidesItsOutputURL(PlatformTestCase):
    """BB-013: nothing dir(toft.run) lists hands back the output URL or the
    job token — called, too, not only read."""

    def test_no_member_returns_a_secret(self):
        with mock.patch.dict(os.environ, {"TOFT_OUTPUT_URI": _URI}):
            run = Run(self.base, _TOKEN, "r1")
        for name in dir(run):
            value = getattr(run, name, None)
            self.assertNotIn("X-Amz", repr(value), name)
            if callable(value) and not isinstance(value, type):
                try:
                    params = inspect.signature(value).parameters
                except (TypeError, ValueError):
                    continue
                if params:
                    continue
                try:
                    got = value()
                except Exception:  # noqa: BLE001
                    continue
                self.assertNotIn("X-Amz", repr(got), name)
                self.assertNotIn(_TOKEN, repr(got), name)
                for attr in dir(got):
                    inner = getattr(got, attr, None)
                    if callable(inner) and attr.startswith("_") and not attr.startswith("__"):
                        try:
                            self.assertNotIn(_TOKEN, repr(inner()), f"{name}().{attr}")
                        except TypeError:
                            pass
        self.assertFalse(hasattr(run, "_output_uri"))
        self.assertFalse(hasattr(run, "_transport"))

    def test_output_and_progress_still_work(self):
        with mock.patch.dict(os.environ, {"TOFT_OUTPUT_URI": self.base + "/store"}):
            run = Run(self.base, _TOKEN, "r1")
        self.route("PUT /store", 200, {})
        run.write_output(b"x")
        self.assertEqual(self.last()["path"], "/store")
        self.route("POST /api/v1/runner/progress", 200, {})
        run.progress(10)
        self.assertEqual(self.last()["headers"].get("Authorization"), f"Bearer {_TOKEN}")


class TestEmptyStorageIsNotOffPlatform(unittest.TestCase):
    """BB-023: TOFT_STORAGE='[]' is not_mapped; only no TOFT_STORAGE is off_platform."""

    def test_empty_list(self):
        env = {k: v for k, v in os.environ.items() if k not in _PLATFORM_VARS}
        with mock.patch.dict(os.environ, {**env, "TOFT_STORAGE": "[]"}, clear=True):
            _refused(self, lambda: toft.storage("x"), "not_mapped")
            _refused(self, lambda: toft.store("x"), "not_mapped")
        with mock.patch.dict(os.environ, env, clear=True):
            _refused(self, lambda: toft.storage("x"), "off_platform")


if __name__ == "__main__":
    unittest.main()
