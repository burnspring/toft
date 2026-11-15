"""The transport's single resend, and a success that is not Toft's API.

BB-002: a 5xx may come from a handler that already did the work, so a write
without an Idempotency-Key is never sent twice. BB-003: a timeout is not
resent, and the whole call is held to one bound. BB-014: a 200 web page is
``not_toft``, coded and not retryable, with a hint.
"""

from __future__ import annotations

import json
import socket
import sys
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import toft  # noqa: E402
from toft import session  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.transport import AppTransport  # noqa: E402


class Script(BaseHTTPRequestHandler):
    calls: list = []
    answer = None  # callable(handler, n) -> None

    def log_message(self, *args):
        pass

    def _any(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        Script.calls.append((self.command, self.path, self.headers.get("Idempotency-Key")))
        Script.answer(self, len(Script.calls))

    do_GET = do_POST = do_PUT = do_DELETE = _any

    def send(self, status, body, content_type="application/json", headers=None):
        raw = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(raw)


class TransportCase(unittest.TestCase):
    timeout = 30.0

    def setUp(self):
        Script.calls = []
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Script)
        self.server.daemon_threads = True
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        host, port = self.server.server_address
        session.reset(AppTransport(base_url=f"http://{host}:{port}", token="t", timeout=self.timeout))
        toft.app.refresh()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        session.reset(None)
        toft.app.refresh()


class FiveHundred(TransportCase):
    def test_an_unkeyed_write_is_not_resent(self):
        Script.answer = lambda h, n: h.send(500, {"error": "boom"}, headers={"Retry-After": "0"})
        calls = {
            "jobs.start": lambda: toft.jobs.start("echo"),
            "data.set": lambda: toft.data.set({"a": 1}),
            "notify.slack": lambda: toft.notify.slack("x"),
        }
        for name, call in calls.items():
            Script.calls = []
            with self.assertRaises(ToftError) as ctx:
                call()
            err = ctx.exception
            self.assertEqual((err.status, err.code, err.retryable), (500, "", True), name)
            self.assertEqual(len(Script.calls), 1, f"{name} was sent twice")

    def test_a_keyed_write_and_a_read_are_resent_once(self):
        def answer(h, n):
            if n % 2 == 1:
                h.send(502, {"error": "gateway"}, headers={"Retry-After": "0"})
            else:
                h.send(202, {"run_id": "run_1", "status": "queued"})
        Script.answer = answer
        toft.jobs.start("echo", idempotency_key="k1")
        self.assertEqual(len(Script.calls), 2)
        self.assertEqual(Script.calls[0][2], Script.calls[1][2])

    def test_a_rate_limit_is_resent_for_an_unkeyed_write(self):
        def answer(h, n):
            if n == 1:
                h.send(429, {"error": "slow", "code": "rate_limited", "retryable": True},
                       headers={"Retry-After": "0"})
            else:
                h.send(200, {"version": 1, "data": {"a": 1}})
        Script.answer = answer
        toft.data.set({"a": 1})
        self.assertEqual(len(Script.calls), 2)

    def test_a_rate_limit_is_waited_out_not_resent_once(self):
        # TOFT-BB-002: a burst past the limiter is paced, not refused.
        def answer(h, n):
            if n <= 3:
                h.send(429, {"error": "slow", "code": "rate_limited", "retryable": True},
                       headers={"Retry-After": "0"})
            else:
                h.send(200, {"version": 1, "data": {"a": 1}})
        Script.answer = answer
        toft.data.set({"a": 1})
        self.assertEqual(len(Script.calls), 4)


class Timeout(TransportCase):
    timeout = 0.3

    def test_a_timeout_is_not_resent(self):
        Script.answer = lambda h, n: time.sleep(1.5)
        start = time.monotonic()
        with self.assertRaises(ToftError) as ctx:
            toft.jobs.start("echo")
        self.assertLess(time.monotonic() - start, 1.2)
        self.assertEqual((ctx.exception.code, ctx.exception.retryable), ("", True))
        self.assertEqual(len(Script.calls), 1)


class RefusedConnection(unittest.TestCase):
    def test_a_connection_that_never_opened_is_resent_for_a_write(self):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()
        from toft import transport as tr
        saved = tr.DEFAULT_RETRY_AFTER
        tr.DEFAULT_RETRY_AFTER = 0.01
        attempts = []
        t = AppTransport(base_url=f"http://127.0.0.1:{port}", token="t")
        real = t._once
        t._once = lambda *a: (attempts.append(1), real(*a))[1]
        try:
            with self.assertRaises(ToftError) as ctx:
                t.post_json("/api/v1/app/jobs/echo/runs", {})
        finally:
            tr.DEFAULT_RETRY_AFTER = saved
        self.assertEqual(len(attempts), 2)
        self.assertEqual((ctx.exception.code, ctx.exception.retryable), ("", True))


class NotToft(TransportCase):
    def test_a_web_page_is_not_toft(self):
        Script.answer = lambda h, n: h.send(200, b"<!doctype html><html>Sign in</html>", "text/html")
        with self.assertRaises(ToftError) as ctx:
            toft.data.get()
        err = ctx.exception
        self.assertEqual((err.code, err.retryable, err.status), ("not_toft", False, 200))
        self.assertTrue(err.hint)
        self.assertIn("web page", str(err))
        self.assertEqual(len(Script.calls), 1)


class CutOffUpload(unittest.TestCase):
    """J-3: a body the server stopped reading is not resent, keyed or not, and
    is not retryable -- the same body would be refused again."""

    def test_a_cut_off_body_is_neither_resent_nor_retryable(self):
        from toft.transport import _resendable, _transport_error

        err = _transport_error("closed the connection before the request was fully sent",
                               sent=True, cut_off=True)
        self.assertFalse(_resendable("POST", {"Idempotency-Key": "k"}, err))
        self.assertFalse(_resendable("GET", {}, err))
        self.assertFalse(err.retryable)
        lost = _transport_error("cannot reach Toft", sent=True)
        self.assertTrue(_resendable("POST", {"Idempotency-Key": "k"}, lost),
                        "a lost answer to a keyed send is still resent")
        self.assertTrue(lost.retryable)


if __name__ == "__main__":
    unittest.main()
