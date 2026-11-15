"""Regressions for the fourth 2026-11-03 black-box list's Python findings
(BB-026, BB-030, BB-042, BB-043, BB-047)."""

from __future__ import annotations

import datetime
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import toft  # noqa: E402
from toft import session  # noqa: E402
from toft.client import Run  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun  # noqa: E402

_PLATFORM_VARS = ("TOFT_API_URL", "TOFT_APP_TOKEN", "TOFT_JOB_TOKEN", "TOFT_APP_ID",
                  "TOFT_RUN_ID", "TOFT_SCRATCH_DIR", "TOFT_STORAGE")


def _off_platform():
    return mock.patch.dict(os.environ, {k: "" for k in _PLATFORM_VARS})


class TestARunPrintsNoSecrets(unittest.TestCase):
    """BB-026: repr/str of a job's run names it without the job token or the
    presigned output URL."""

    def test_repr_and_str(self):
        env = {"TOFT_OUTPUT_URI": "https://bucket/out?X-Amz-Credential=AKIA",
               "TOFT_JOB_NAME": "nightly", "TOFT_TRIGGER": "ui"}
        with mock.patch.dict(os.environ, env):
            run = Run("http://toft", "job-secret-token", "r1")
        for printed in (repr(run), str(run)):
            self.assertNotIn("job-secret-token", printed)
            self.assertNotIn("X-Amz", printed)
            self.assertIn("r1", printed)


class TestRespondCarriesEmptyFieldsAsNull(unittest.TestCase):
    """BB-030: the 202 body carries rerun_of, scheduled_for, deadline,
    exit_code, failure_reason and deduplicated even when the platform left
    them out."""

    def test_to_dict(self):
        run = JobRun.from_raw({"id": "r1", "job": "j", "status": "queued",
                                "status_url": "https://apps.example.test/apps/demo/_toft/runs/r1"})
        body, status, _ = toft.jobs.respond(run, request={"Accept": "application/json"})
        self.assertEqual(status, 202)
        decoded = json.loads(body)
        for key in ("rerun_of", "scheduled_for", "deadline", "exit_code", "failure_reason"):
            self.assertIn(key, decoded)
            self.assertIsNone(decoded[key])
        self.assertIs(decoded["deduplicated"], False)

    def test_a_set_field_keeps_its_text(self):
        # BB-045: the record's text, offset and all — never normalised to UTC.
        run = JobRun.from_raw({"id": "r1", "scheduled_for": "2026-11-03T09:00:00-05:00"})
        # BB-050/D-33: the field is now an aware datetime, in the sent offset.
        self.assertEqual(run.scheduled_for_text, "2026-11-03T09:00:00-05:00")
        self.assertEqual(run.scheduled_for.utcoffset(), datetime.timedelta(hours=-5))
        self.assertEqual(run.to_dict()["scheduled_for"], "2026-11-03T09:00:00-05:00")


class TestStorageEdgeCases(unittest.TestCase):
    """BB-043: a folder with a '..' segment is invalid_name; an entry missing
    its kind or access is storage_env_invalid."""

    def test_a_traversing_folder_is_invalid_name(self):
        env = {"TOFT_STORAGE": json.dumps(
            [{"name": "v", "kind": "volume", "path": "/data/v", "access": "full"}])}
        with mock.patch.dict(os.environ, env):
            for folder in ("..", "a/../b", "/abs", "a/"):
                with self.assertRaises(ToftError) as caught:
                    toft.storage("v", folder=folder)
                self.assertEqual(caught.exception.code, "invalid_name", folder)
                self.assertTrue(caught.exception.hint)
            self.assertEqual(toft.storage("v", folder="").name, "v")

    def test_an_incomplete_entry_is_refused(self):
        for entries in ([{"name": "x"}], [{"name": "x", "kind": "store"}],
                        [{"kind": "store", "access": "read"}]):
            with mock.patch.dict(os.environ, {"TOFT_STORAGE": json.dumps(entries)}):
                with self.assertRaises(ToftError) as caught:
                    toft.mounts()
                self.assertEqual(caught.exception.code, "storage_env_invalid")
                self.assertTrue(caught.exception.hint)


class TestNaNIsBadRequestOffPlatform(unittest.TestCase):
    """BB-047: a value JSON cannot hold is bad_request before off_platform."""

    def test_data_set_and_jobs_start(self):
        with _off_platform():
            session.reset(None)
            for call in (lambda: toft.data.set({"x": float("nan")}),
                         lambda: toft.jobs.start("j", params={"x": float("inf")})):
                with self.assertRaises(ToftError) as caught:
                    call()
                self.assertEqual(caught.exception.code, "bad_request")


class _Platform(BaseHTTPRequestHandler):
    def _answer(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        if self.path.startswith("/api/v1/app/data"):
            status, body = 412, {"code": "version_conflict", "error": "stale", "version": 4}
        elif self.path == "/store":
            status, body = 403, None
        else:
            status, body = 404, {"code": "not_found", "error": "no"}
        data = json.dumps(body).encode() if body is not None else b""
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    do_GET = do_PUT = do_POST = _answer

    def log_message(self, *args):
        pass


class TestSDKRefusalsCarryAHint(unittest.TestCase):
    """BB-042: the refusals the SDK makes or shapes itself carry a hint."""

    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), _Platform)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.url = f"http://127.0.0.1:{cls.server.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def hinted(self, call, code):
        with self.assertRaises(ToftError) as caught:
            call()
        self.assertEqual(caught.exception.code, code)
        self.assertTrue(caught.exception.hint, f"{code} has no hint")

    def test_hints(self):
        env = {"TOFT_API_URL": self.url, "TOFT_APP_TOKEN": "tok", "TOFT_APP_ID": "a1"}
        with mock.patch.dict(os.environ, env):
            session.reset(None)
            self.hinted(lambda: toft.notify.email(to=[5], subject="s", text="t"), "bad_request")
            self.hinted(lambda: toft.notify.slack("   "), "bad_request")
            self.hinted(lambda: toft.notify.email(to="a@example.com", subject=" ", text="t"),
                        "bad_request")
            self.hinted(lambda: toft.data.set({"a": 1}, if_version=3), "version_conflict")
        with mock.patch.dict(os.environ, {"TOFT_OUTPUT_URI": self.url + "/store"}):
            run = Run(self.url, "job-token", "r1")
        self.hinted(lambda: run.write_output(b"x"), "output_unwritable")
        session.reset(None)


if __name__ == "__main__":
    unittest.main()
