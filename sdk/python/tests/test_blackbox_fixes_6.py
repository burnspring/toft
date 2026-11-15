"""Regressions for the sixth 2026-11-03 black-box list's Python findings
(BB-076, BB-079, BB-080)."""

from __future__ import annotations

import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft import session  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun  # noqa: E402
from toft.resources import mounts, storage  # noqa: E402

from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402


def _hinted(test, call, code):
    with test.assertRaises(ToftError) as caught:
        call()
    test.assertEqual(caught.exception.code, code, str(caught.exception))
    test.assertTrue(caught.exception.hint, f"{code} ({caught.exception}) has no hint")
    return caught.exception


class TestDataUpdateTakesAFunction(PlatformTestCase):
    """BB-079: a dict handed to update() is a coded bad_request, refused before
    any request, not "'dict' object is not callable"."""

    def test_dict_is_bad_request_before_any_request(self):
        for wrong in ({"a": 1}, None, "x", 3):
            FakePlatform.requests = []
            err = _hinted(self, lambda: toft.data.update(wrong), "bad_request")
            self.assertIn("function", err.hint)
            self.assertIn("set(", err.hint)
            self.assertEqual(FakePlatform.requests, [], f"update({wrong!r}) made a request")

    def test_off_platform_is_still_bad_request(self):
        session.reset(None)
        with mock.patch.dict(os.environ, {}, clear=False):
            for var in ("TOFT_API_URL", "TOFT_APP_TOKEN"):
                os.environ.pop(var, None)
            _hinted(self, lambda: toft.data.update({"a": 1}), "bad_request")


class TestStorageEnvEntriesAreValidated(unittest.TestCase):
    """BB-080: non-object entries and unknown kinds or access levels are
    storage_env_invalid, not dropped or accepted."""

    def tearDown(self):
        os.environ.pop("TOFT_STORAGE", None)

    def test_non_object_entries_refused(self):
        for raw in ("[5]", "[null]", '["x"]', '[[]]', '[true]',
                    '[{"name":"x","kind":"volume","path":"/d","access":"full"}, 5]'):
            with mock.patch.dict(os.environ, {"TOFT_STORAGE": raw}):
                _hinted(self, mounts, "storage_env_invalid")
                _hinted(self, lambda: storage("x"), "storage_env_invalid")

    def test_unknown_kind_and_access_refused(self):
        for raw in ('[{"name":"x","kind":"bogus","path":"/d","access":"full"}]',
                    '[{"name":"x","kind":"volume","path":"/d","access":"bogus"}]',
                    '[{"name":"x","kind":"volume","path":"/d","access":"readwrite"}]',
                    '[{"name":"x","kind":"Volume","path":"/d","access":"full"}]'):
            with mock.patch.dict(os.environ, {"TOFT_STORAGE": raw}):
                err = _hinted(self, mounts, "storage_env_invalid")
                self.assertIn("volume or store", err.hint)
                self.assertIn("read, write or full", err.hint)

    def test_valid_entries_still_read(self):
        raw = ('[{"name":"v","kind":"volume","path":"/d","access":"write"},'
               '{"name":"s","kind":"store","path":"","access":"read"}]')
        with mock.patch.dict(os.environ, {"TOFT_STORAGE": raw}):
            got = mounts()
        self.assertEqual([(m.name, m.kind, m.access) for m in got],
                         [("v", "volume", "write"), ("s", "store", "read")])


class TestRunCarriesThePlatformsNewFields(unittest.TestCase):
    """BB-076: cpu_throttled_seconds, execution_mode and definition_id."""

    def test_present(self):
        run = JobRun.from_raw({"id": "r1", "cpu_throttled_seconds": 1.5,
                               "execution_mode": "isolated", "definition_id": "d1"})
        self.assertEqual(run.cpu_throttled_seconds, 1.5)
        self.assertEqual(run.execution_mode, "isolated")
        self.assertEqual(run.definition_id, "d1")
        out = run.to_dict()
        self.assertEqual(out["cpu_throttled_seconds"], 1.5)
        self.assertEqual(out["execution_mode"], "isolated")
        self.assertEqual(out["definition_id"], "d1")

    def test_integer_seconds_is_a_float(self):
        run = JobRun.from_raw({"id": "r1", "cpu_throttled_seconds": 0})
        self.assertEqual(run.cpu_throttled_seconds, 0.0)
        self.assertIsInstance(run.cpu_throttled_seconds, float)

    def test_absent_is_none(self):
        run = JobRun.from_raw({"id": "r1"})
        self.assertIsNone(run.cpu_throttled_seconds)
        self.assertIsNone(run.execution_mode)
        self.assertIsNone(run.definition_id)


if __name__ == "__main__":
    unittest.main()
