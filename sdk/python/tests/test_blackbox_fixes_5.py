"""Regressions for the fifth 2026-11-03 black-box list's Python findings
(BB-050, BB-053, BB-057, BB-061, D-33, D-34)."""

from __future__ import annotations

import copy
import datetime
import os
import pickle
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft import session  # noqa: E402
from toft.client import Run  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun  # noqa: E402
from toft.resources import Mount, mounts  # noqa: E402

from test_app_sdk import PlatformTestCase  # noqa: E402

_PLATFORM_VARS = ("TOFT_API_URL", "TOFT_APP_TOKEN", "TOFT_JOB_TOKEN", "TOFT_APP_ID",
                  "TOFT_RUN_ID", "TOFT_SCRATCH_DIR", "TOFT_STORAGE")

_TOKEN = "job-secret-token-9f3a"
_URI = "https://bucket.s3.amazonaws.com/out?X-Amz-Credential=AKIAXYZ&X-Amz-Security-Token=SESSIONTOK"


def _hinted(test, call, code):
    with test.assertRaises(ToftError) as caught:
        call()
    test.assertEqual(caught.exception.code, code, str(caught.exception))
    test.assertTrue(caught.exception.hint, f"{code} ({caught.exception}) has no hint")
    return caught.exception


class TestRunIntrospectionHoldsNoSecrets(PlatformTestCase):
    """BB-057: vars(), dir()'s values and pickle never reach the run token or
    the presigned output URL — and the run still works."""

    def run_(self):
        with mock.patch.dict(os.environ, {"TOFT_OUTPUT_URI": _URI, "TOFT_JOB_NAME": "nightly"}):
            return Run(self.base, _TOKEN, "r1")

    def assertClean(self, text):
        self.assertNotIn(_TOKEN, text)
        self.assertNotIn("X-Amz", text)
        self.assertNotIn("SESSIONTOK", text)

    def test_vars_and_dir(self):
        run = self.run_()
        self.assertClean(repr(vars(run)))
        for name in dir(run):
            try:
                value = getattr(run, name)
            except Exception:  # noqa: BLE001
                continue
            self.assertClean(repr(value))
            if hasattr(value, "__dict__"):
                self.assertClean(repr(vars(value)))
        self.assertClean(repr(run))
        self.assertClean(str(run))

    def test_pickle_is_refused_with_a_hint(self):
        run = self.run_()
        for protocol in range(pickle.HIGHEST_PROTOCOL + 1):
            err = _hinted(self, lambda: pickle.dumps(run, protocol), "bad_request")
            self.assertClean(str(err))
        self.assertIs(copy.copy(run), run)
        self.assertIs(copy.deepcopy(run), run)

    def test_progress_still_carries_the_token(self):
        self.route("POST /api/v1/runner/progress", 200, {})
        self.run_().progress(40, "loading")
        self.assertEqual(self.last()["headers"].get("Authorization"), f"Bearer {_TOKEN}")

    def test_output_still_uses_the_uri(self):
        with mock.patch.dict(os.environ, {"TOFT_OUTPUT_URI": self.base + "/store"}):
            run = Run(self.base, _TOKEN, "r1")
        self.route("PUT /store", 200, {})
        run.write_output(b"x")
        self.assertEqual(self.last()["path"], "/store")


class TestRefusalsCarryAHint(PlatformTestCase):
    """BB-053: each of these refusals says what to do next."""

    def test_local_refusals(self):
        _hinted(self, lambda: toft.session_scratch_dir(""), "bad_request")
        _hinted(self, lambda: toft.jobs.start(""), "bad_request")
        _hinted(self, lambda: toft.jobs.get(""), "bad_request")
        _hinted(self, lambda: toft.data.set([1, 2]), "bad_request")
        _hinted(self, lambda: Run(self.base, "t", "r1").progress(101), "bad_request")

    def test_an_unknown_run_is_not_found_with_a_hint(self):
        self.route("GET /api/v1/app/job-runs/nope", 404,
                   {"error": "no such run", "code": "not_found", "retryable": False})
        err = _hinted(self, lambda: toft.jobs.get("nope"), "not_found")
        self.assertIn("toft.jobs.list()", err.hint)
        self.assertEqual(err.status, 404)

    def test_the_platforms_own_hint_wins(self):
        self.route("GET /api/v1/app/job-runs/nope", 404,
                   {"error": "no such run", "code": "not_found", "retryable": False, "hint": "theirs"})
        self.assertEqual(_hinted(self, lambda: toft.jobs.get("nope"), "not_found").hint, "theirs")

    def test_off_platform(self):
        with mock.patch.dict(os.environ, {k: "" for k in _PLATFORM_VARS}):
            session.reset(None)
            try:
                err = _hinted(self, lambda: toft.data.read(), "off_platform")
                self.assertIn("TOFT_API_URL is not set", str(err))
                _hinted(self, Run.from_env, "off_platform")
            finally:
                session.reset(None)

    def test_storage_env_invalid(self):
        for raw in ("{not json", '{"name": "x"}', '"x"'):
            with mock.patch.dict(os.environ, {"TOFT_STORAGE": raw}):
                _hinted(self, mounts, "storage_env_invalid")


class TestStorageEnvHintDoesNotRequirePath(unittest.TestCase):
    """BB-061: path is optional — a store over signed URLs has none."""

    def test_hint(self):
        with mock.patch.dict(os.environ, {"TOFT_STORAGE": '[{"name": "x", "kind": "store"}]'}):
            err = _hinted(self, mounts, "storage_env_invalid")
        self.assertIn("path is optional", err.hint)
        self.assertNotIn("path and access", err.hint)
        # And the code agrees: an entry without a path is accepted.
        with mock.patch.dict(os.environ, {"TOFT_STORAGE":
                                          '[{"name": "x", "kind": "store", "access": "read"}]'}):
            self.assertFalse(mounts()[0].mounted)


class TestJobRunScheduledForIsADatetime(unittest.TestCase):
    """BB-050 / D-33: JobRun.scheduled_for is the type toft.run.scheduled_for is."""

    def test_offset_kept(self):
        run = JobRun.from_raw({"id": "r1", "scheduled_for": "2026-11-03T12:34:00-05:00"})
        self.assertIsInstance(run.scheduled_for, datetime.datetime)
        self.assertEqual(run.scheduled_for.utcoffset(), datetime.timedelta(hours=-5))
        self.assertEqual(run.scheduled_for_text, "2026-11-03T12:34:00-05:00")
        self.assertEqual(run.to_dict()["scheduled_for"], "2026-11-03T12:34:00-05:00")
        with mock.patch.dict(os.environ, {"TOFT_SCHEDULED_FOR": "2026-11-03T12:34:00-05:00"}):
            self.assertEqual(type(Run("http://x", "t", "r").scheduled_for), type(run.scheduled_for))

    def test_none(self):
        run = JobRun.from_raw({"id": "r1"})
        self.assertIsNone(run.scheduled_for)
        self.assertIsNone(run.scheduled_for_text)


class TestMountReprSaysPath(unittest.TestCase):
    """D-34: the repr names the attribute app code reads."""

    def test_repr(self):
        text = repr(Mount(name="x", kind="volume", mount_path="/storage/x", access="read"))
        self.assertIn("path='/storage/x'", text)
        self.assertNotIn("mount_path", text)
        self.assertIn("path=''", repr(Mount(name="s", kind="store", access="read")))
        self.assertIn("read_only=True", text)
        self.assertIn("mounted=True", text)


class TestMountSerialisesLikeTheOtherThree(unittest.TestCase):
    """BB-044 parity, found by the 2026-11-03 evening drills: Node, Go and R
    serialise a mount with read_only and mounted; Python's had no form at all."""

    def test_to_dict(self):
        import json
        m = Mount(name="s", kind="store", access="write", folder="backup")
        self.assertEqual(json.loads(json.dumps(m.to_dict())),
                         {"name": "s", "kind": "store", "path": "", "access": "write",
                          "folder": "backup", "read_only": False, "mounted": False})


if __name__ == "__main__":
    unittest.main()
