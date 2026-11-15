"""A job's own calls read a refusal the way the app's do."""

from __future__ import annotations

import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from toft.client import Run  # noqa: E402
from toft.errors import ToftError  # noqa: E402


class _Refuse(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        body = json.dumps({"error": "skew", "code": "contract_skew",
                           "retryable": False, "hint": "upgrade the SDK"}).encode()
        self.send_response(409)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


class TestRunRefusalCarriesTheEnvelope(unittest.TestCase):
    def test_progress_refusal(self):
        server = HTTPServer(("127.0.0.1", 0), _Refuse)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            run = Run(f"http://127.0.0.1:{server.server_port}", "t", "r")
            with self.assertRaises(ToftError) as caught:
                run.progress(50)
            err = caught.exception
            self.assertEqual(err.code, "contract_skew")
            self.assertEqual(err.hint, "upgrade the SDK")
            self.assertFalse(err.retryable)
        finally:
            server.shutdown()


class _Record(BaseHTTPRequestHandler):
    seen: list = []

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        _Record.seen.append((self.path, json.loads(body)))
        self.send_response(204)
        self.end_headers()

    def log_message(self, *args):
        pass


class TestProgressCarriesAMessage(unittest.TestCase):
    def test_message_rides_beside_the_percent(self):
        server = HTTPServer(("127.0.0.1", 0), _Record)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            run = Run(f"http://127.0.0.1:{server.server_port}", "t", "r")
            run.progress(40, "reading ledgers")
            run.progress(50)
        finally:
            server.shutdown()
        (path, first), (_, second) = _Record.seen
        self.assertEqual(path, "/api/v1/runner/progress")
        self.assertEqual((first["percent"], first["message"]), (40, "reading ledgers"))
        # No message, no key: the wire stays what a percent-only report was.
        self.assertNotIn("message", second)


if __name__ == "__main__":
    unittest.main()
