"""Regressions for the third 2026-11-03 black-box list's Python findings
(BB-008, BB-020, BB-022, BB-024)."""

from __future__ import annotations

import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft.caller import Caller  # noqa: E402
from toft.client import Run  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun  # noqa: E402
from toft.resources import _send  # noqa: E402


class _Store(BaseHTTPRequestHandler):
    def _answer(self):
        self.send_response(403 if self.path == "/forbidden" else 503)
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_PUT = do_GET = _answer

    def log_message(self, *args):
        pass


class TestStoreRefusalsAreCoded(unittest.TestCase):
    """BB-008: an object store's refusal is coded, retryable only for a 5xx;
    only a store that cannot be reached is uncoded."""

    def setUp(self):
        self.server = HTTPServer(("127.0.0.1", 0), _Store)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        host, port = self.server.server_address
        self.base = f"http://{host}:{port}"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()

    def _run(self, uri):
        with mock.patch.dict(os.environ, {"TOFT_OUTPUT_URI": uri}):
            return Run("http://127.0.0.1:1", "t", "r1")

    def test_write_output(self):
        for uri, code, retryable in (
            (self.base + "/forbidden", "output_unwritable", False),
            (self.base + "/down", "output_unwritable", True),
            ("http://127.0.0.1:1/out", "", True),
        ):
            with self.assertRaises(ToftError) as ctx:
                self._run(uri).write_output(b"x")
            self.assertEqual((ctx.exception.code, ctx.exception.retryable), (code, retryable), uri)

    def test_signed_request(self):
        with self.assertRaises(ToftError) as ctx:
            _send({"url": self.base + "/forbidden", "method": "GET"}, None)
        self.assertEqual((ctx.exception.code, ctx.exception.retryable, ctx.exception.status),
                         ("store_refused", False, 403))


class TestEmptyOutputIsNotJSON(unittest.TestCase):
    """BB-022: an output written empty is not_json, not None."""

    def test_empty(self):
        run = JobRun.from_raw({"id": "run_1"})
        with mock.patch.object(JobRun, "output", return_value=b""):
            with self.assertRaises(ToftError) as ctx:
                run.output_json()
        self.assertEqual(ctx.exception.code, "not_json")


class TestAtLeastRefusesAWordThatIsNotARole(unittest.TestCase):
    """BB-024: a misspelt role is bad_request, not a silent False."""

    def test_unknown_words(self):
        for word in ("superuser", "", "Editor", None):
            with self.assertRaises(ToftError) as ctx:
                Caller("owner").at_least(word)
            self.assertEqual(ctx.exception.code, "bad_request")

    def test_roles_off_the_ladder_answer_false(self):
        for word in ("system", "app", "unknown"):
            self.assertFalse(Caller("owner").at_least(word))


class TestGetNamesARunId(unittest.TestCase):
    """BB-020: jobs.get("") says it needs a run id, not a name."""

    def test_get_empty(self):
        with self.assertRaises(ToftError) as ctx:
            toft.jobs.get("")
        self.assertIn("run id", str(ctx.exception))
        self.assertEqual(ctx.exception.code, "bad_request")


if __name__ == "__main__":
    unittest.main()
