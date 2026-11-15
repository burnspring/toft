"""Contract tests for the app surface, against a fake platform.

Standard library only, like the SDK: ``python3 -m unittest discover -s tests``
from ``sdk/python`` runs them with nothing installed.

The scenarios here are the ones the Node SDK must also pass — same routes, same
headers, same idempotency behaviour — so that an app written in one language
cannot observe a platform the other cannot.
"""

from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile
import threading
import unittest
from unittest import mock
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import toft  # noqa: E402
from toft import session  # noqa: E402
from toft.env import APP_CONTRACT_VERSION  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.viewer import _reset_identity_cache  # noqa: E402
from toft.transport import AppTransport, Part, encode_multipart  # noqa: E402


class FakePlatform(BaseHTTPRequestHandler):
    """Records every request and replies from a scripted table."""

    requests: list = []
    routes: dict = {}

    def log_message(self, *args):  # silence the default stderr spam
        pass

    def _record(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        FakePlatform.requests.append(
            {
                "method": self.command,
                "path": self.path,
                "headers": dict(self.headers),
                "body": body,
            }
        )
        return body

    def _respond(self):
        self._record()
        key = f"{self.command} {self.path.split('?')[0]}"
        script = FakePlatform.routes.get(key)
        if script is None:
            self.send_response(404)
            self.end_headers()
            self.wfile.write(b'{"error":"no such route"}')
            return
        # A list means "answer these in order" — how a retry is scripted.
        if isinstance(script, list):
            entry = script.pop(0) if len(script) > 1 else script[0]
        else:
            entry = script
        status, payload, headers = entry
        self.send_response(status)
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.end_headers()
        # 204 carries no body, and the SDK reads that absence as its answer —
        # writing "null" here would test a shape the platform never sends.
        if status != 204:
            self.wfile.write(json.dumps(payload).encode())

    do_GET = _respond
    do_POST = _respond
    do_PUT = _respond
    do_DELETE = _respond


class PlatformTestCase(unittest.TestCase):
    def setUp(self):
        FakePlatform.requests = []
        FakePlatform.routes = {}
        self.server = HTTPServer(("127.0.0.1", 0), FakePlatform)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        host, port = self.server.server_address
        self.base = f"http://{host}:{port}"
        session.reset(AppTransport(base_url=self.base, token="tofta_test"))
        toft.app.refresh()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        session.reset(None)
        toft.app.refresh()
        os.environ.pop("TOFT_STORAGE", None)

    def route(self, key, status, payload, headers=None):
        FakePlatform.routes[key] = (status, payload, headers)

    def script(self, key, entries):
        FakePlatform.routes[key] = list(entries)

    def last(self):
        return FakePlatform.requests[-1]


class TestIdentity(PlatformTestCase):
    def test_self_is_fetched_once_and_cached(self):
        self.route(
            "GET /api/v1/app/self",
            200,
            {
                "id": "app_1",
                "slug": "reporter",
                "name": "Weekly Reporter",
                "mode": "shared",
                "capabilities": ["slack.post", "email.send"],
                "policy": {"keep_versions": 5},
            },
        )
        self.assertEqual(toft.app.slug, "reporter")
        self.assertEqual(toft.app.name, "Weekly Reporter")
        self.assertEqual(toft.app.mode, "shared")
        self.assertEqual(toft.app.policy["keep_versions"], 5)
        # Every property read above must not have cost a request.
        self.assertEqual(len(FakePlatform.requests), 1)

    def test_can_accepts_an_action_or_an_integration_name(self):
        self.route(
            "GET /api/v1/app/self",
            200,
            {"id": "a", "slug": "s", "capabilities": ["slack.post", "email.send"]},
        )
        self.assertTrue(toft.app.can("email.send"))
        self.assertTrue(toft.app.can("email"))       # the integration, any action
        self.assertTrue(toft.app.can("slack"))
        # An action not in the list is false. The platform now grants
        # slack.file alongside slack.post, so this list is a contrived one —
        # what is under test is the matcher, which must not treat "slack.post"
        # as covering every slack.* action.
        self.assertFalse(toft.app.can("slack.file"))
        self.assertFalse(toft.app.can("webhook"))
        # An empty name is a mistake, not a capability nobody holds (BB report).
        with self.assertRaises(ToftError) as ctx:
            toft.app.can("")
        self.assertEqual(ctx.exception.code, "bad_request")

    def test_base_path_is_what_the_platform_sent(self):
        self.route(
            "GET /api/v1/app/self",
            200,
            {"id": "a", "slug": "reporter", "base_path": "/apps/reporter"},
        )
        self.assertEqual(toft.app.base_path, "/apps/reporter")

    def test_base_path_falls_back_to_the_injected_variable(self):
        """An install older than the field must not be reported as "not mounted".

        The accessor used to answer "" there and report it as fact, and an app
        building its links from it emitted root-absolute URLs — a 404 on every
        link, with nothing in any log saying why. "" is also the honest answer
        on a per-app-origin install, so nothing an app could check told the two
        apart.
        """
        self.route("GET /api/v1/app/self", 200, {"id": "a", "slug": "reporter"})
        os.environ["BASE_PATH"] = "/apps/reporter"
        try:
            self.assertEqual(toft.app.base_path, "/apps/reporter")
        finally:
            os.environ.pop("BASE_PATH", None)

    def test_base_path_is_empty_when_neither_half_says_otherwise(self):
        self.route("GET /api/v1/app/self", 200, {"id": "a", "slug": "reporter"})
        os.environ.pop("BASE_PATH", None)
        self.assertEqual(toft.app.base_path, "")

    def test_contract_version_is_sent(self):
        self.route("GET /api/v1/app/self", 200, {"id": "a", "slug": "s"})
        toft.app.id
        self.assertEqual(
            self.last()["headers"].get("X-Toft-App-Contract"), APP_CONTRACT_VERSION
        )
        self.assertEqual(self.last()["headers"].get("Authorization"), "Bearer tofta_test")


class TestSlack(PlatformTestCase):
    def test_post_returns_the_record_id(self):
        self.route("POST /api/v1/app/notify/slack", 202, {"send_id": "snd_1", "status": "queued"})
        send = toft.notify.slack("Ingest finished: 1,204 rows")
        self.assertEqual(send.id, "snd_1")
        self.assertEqual(send.status, "queued")
        self.assertFalse(send.done)
        body = json.loads(self.last()["body"])
        self.assertEqual(body, {"text": "Ingest finished: 1,204 rows"})

    def test_post_sends_no_idempotency_key_by_default(self):
        self.route("POST /api/v1/app/notify/slack", 202, {"send_id": "s", "status": "queued"})
        toft.notify.slack("hello")
        self.assertNotIn("Idempotency-Key", self.last()["headers"])

    def test_empty_text_is_refused_locally(self):
        with self.assertRaises(ToftError):
            toft.notify.slack("   ")

    def test_file_upload_is_multipart_with_a_key(self):
        self.route("POST /api/v1/app/notify/slack/file", 202, {"send_id": "s2", "status": "queued"})
        send = toft.notify.slack_file(
            ("recon.xlsx", b"binary-bytes"), title="Weekly reconciliation", comment="3 breaks"
        )
        self.assertEqual(send.id, "s2")
        req = self.last()
        self.assertTrue(req["headers"]["Content-Type"].startswith("multipart/form-data"))
        self.assertIn("Idempotency-Key", req["headers"])
        self.assertIn(b'filename="recon.xlsx"', req["body"])
        self.assertIn(b"binary-bytes", req["body"])
        self.assertIn(b"Weekly reconciliation", req["body"])

    def test_a_refusal_carries_the_platform_wording(self):
        self.route(
            "POST /api/v1/app/notify/slack",
            403,
            {"error": "Slack is not enabled for this app — an administrator can "
                      "enable it on the app's Integrations tab"},
        )
        with self.assertRaises(ToftError) as caught:
            toft.notify.slack("hello")
        self.assertIn("Integrations tab", str(caught.exception))
        self.assertEqual(caught.exception.status, 403)
        self.assertFalse(caught.exception.retryable)


class TestErrorEnvelope(PlatformTestCase):
    """Whether to try again is the platform's call.

    Every error body it writes carries ``retryable``, because only the handler
    knows that a 503 means "this install is configured without it" or that a
    409 means "wait"; the status rule is the fallback for answers that never
    reached a handler. The same table runs in Go, Node and R.
    """

    def test_the_bodys_retryable_wins_over_the_status(self):
        cases = [
            (503, {"code": "jobs_unavailable", "retryable": False}, False),
            (409, {"code": "queue_full", "retryable": True}, True),
            (503, {"error": "x"}, True),
            (409, {"error": "x"}, False),
            (429, {}, True),
            (None, {}, True),
            (400, {"retryable": "yes"}, False),
        ]
        for status, body, want in cases:
            with self.subTest(status=status, body=body):
                self.assertEqual(ToftError("x", status=status, body=body).retryable, want)

    def test_hint_is_the_platforms_next_step(self):
        exc = ToftError("x", status=401, body={"code": "key_expired", "hint": "mint a new key"})
        self.assertEqual((exc.code, exc.hint), ("key_expired", "mint a new key"))
        self.assertIsNone(ToftError("x", status=404).hint)

    def test_the_built_in_retry_honours_the_platforms_veto(self):
        self.route("POST /api/v1/app/notify/slack", 503,
                   {"error": "not configured", "code": "unavailable", "retryable": False,
                    "hint": "an administrator configures it"})
        with self.assertRaises(ToftError) as caught:
            toft.notify.slack("hello")
        self.assertFalse(caught.exception.retryable)
        self.assertEqual(caught.exception.hint, "an administrator configures it")
        self.assertEqual(len(FakePlatform.requests), 1)

    def test_a_retryable_4xx_is_the_callers_to_retry(self):
        self.route("POST /api/v1/app/notify/slack", 409,
                   {"error": "wait", "code": "busy", "retryable": True})
        with self.assertRaises(ToftError) as caught:
            toft.notify.slack("hello")
        self.assertTrue(caught.exception.retryable)
        self.assertEqual(len(FakePlatform.requests), 1)


class TestEmail(PlatformTestCase):
    def test_a_single_address_is_accepted_as_a_string(self):
        self.route("POST /api/v1/app/notify/email", 202, {"send_id": "e1", "status": "queued"})
        toft.notify.email(to="finance@example.com", subject="Report", text="body")
        body = json.loads(self.last()["body"])
        self.assertEqual(body["to"], ["finance@example.com"])
        self.assertEqual(body["subject"], "Report")

    def test_an_idempotency_key_is_generated_by_default(self):
        self.route("POST /api/v1/app/notify/email", 202, {"send_id": "e", "status": "queued"})
        toft.notify.email(to=["a@example.com"], subject="s", text="t")
        self.assertTrue(self.last()["headers"].get("Idempotency-Key"))

    def test_a_callers_key_is_used_verbatim(self):
        self.route("POST /api/v1/app/notify/email", 202, {"send_id": "e", "status": "queued"})
        toft.notify.email(
            to=["a@example.com"], subject="s", text="t", idempotency_key="weekly-2026-W32"
        )
        self.assertEqual(self.last()["headers"]["Idempotency-Key"], "weekly-2026-W32")

    def test_no_recipients_is_refused_before_the_network(self):
        with self.assertRaises(ToftError):
            toft.notify.email(to=[], subject="s", text="t")
        self.assertEqual(FakePlatform.requests, [])

    def test_attachments_switch_the_request_to_multipart(self):
        self.route("POST /api/v1/app/notify/email", 202, {"send_id": "e2", "status": "queued"})
        toft.notify.email(
            to=["a@example.com"],
            subject="Weekly reconciliation",
            text="3 breaks this week.",
            attachments=[("recon.csv", "a,b,c\n")],
        )
        req = self.last()
        self.assertTrue(req["headers"]["Content-Type"].startswith("multipart/form-data"))
        # The message itself rides as one JSON field, not as base64 in a body.
        self.assertIn(b'name="payload"', req["body"])
        self.assertIn(b'filename="recon.csv"', req["body"])
        payload = req["body"].split(b'name="payload"')[1].split(b"\r\n\r\n")[1].split(b"\r\n--")[0]
        self.assertEqual(json.loads(payload)["subject"], "Weekly reconciliation")


class TestDeliverySemantics(PlatformTestCase):
    def test_a_429_is_retried_once_with_the_same_idempotency_key(self):
        self.script(
            "POST /api/v1/app/notify/email",
            [
                (429, {"error": "too many sends"}, {"Retry-After": "0"}),
                (202, {"send_id": "e9", "status": "queued"}, None),
            ],
        )
        send = toft.notify.email(to=["a@example.com"], subject="s", text="t")
        self.assertEqual(send.id, "e9")
        self.assertEqual(len(FakePlatform.requests), 2)
        first, second = FakePlatform.requests
        # The whole point: the retry must not be able to send a second message.
        self.assertEqual(first["headers"]["Idempotency-Key"], second["headers"]["Idempotency-Key"])

    def test_a_400_is_not_retried(self):
        self.route("POST /api/v1/app/notify/slack", 400, {"error": "text is too long"})
        with self.assertRaises(ToftError):
            toft.notify.slack("x")
        self.assertEqual(len(FakePlatform.requests), 1)

    def test_an_idempotent_replay_reports_the_original_send(self):
        self.route("POST /api/v1/app/notify/email", 200, {"send_id": "e1", "status": "sent"})
        send = toft.notify.email(
            to=["a@example.com"], subject="s", text="t", idempotency_key="k"
        )
        self.assertEqual(send.id, "e1")
        self.assertTrue(send.delivered)

    def test_status_reads_one_send_back(self):
        self.route(
            "GET /api/v1/app/sends/snd_1",
            200,
            {"id": "snd_1", "status": "blocked", "integration": "email", "error": "detached"},
        )
        got = toft.notify.status("snd_1")
        self.assertTrue(got.blocked)
        self.assertTrue(got.done)
        self.assertEqual(got.error, "detached")

    def test_status_accepts_the_send_it_was_given(self):
        self.route("POST /api/v1/app/notify/slack", 202, {"send_id": "s1", "status": "queued"})
        self.route("GET /api/v1/app/sends/s1", 200, {"id": "s1", "status": "sent"})
        send = toft.notify.slack("hello")
        self.assertTrue(toft.notify.status(send).delivered)


class TestAccessTokens(PlatformTestCase):
    def setUp(self):
        super().setUp()
        from toft.tokens import _reset
        _reset()

    def _issue(self, minutes):
        from datetime import datetime, timedelta, timezone
        expires = (datetime.now(timezone.utc) + timedelta(minutes=minutes)).strftime(
            "%Y-%m-%dT%H:%M:%SZ")
        self.route("POST /api/v1/app/tokens/sharepoint", 200, {
            "access_token": "eyJ.tok", "token_type": "Bearer", "expires_at": expires,
            "resource": "https://graph.microsoft.com",
            "folder": {"drive_id": "b!d", "item_id": "01R", "label": "reports"},
        })

    def test_a_token_is_fetched_once_and_cached(self):
        self._issue(60)
        first = toft.access_token("sharepoint")
        again = toft.access_token("sharepoint")
        self.assertEqual(first.value, "eyJ.tok")
        self.assertEqual(first.folder.drive_id, "b!d")
        self.assertEqual(first.folder.label, "reports")
        self.assertIs(first, again)
        self.assertEqual(len(FakePlatform.requests), 1)
        self.assertEqual(self.last()["method"], "POST")
        self.assertNotIn("eyJ", repr(first))

    def test_a_token_near_expiry_is_replaced(self):
        self._issue(4)
        toft.access_token("sharepoint")
        toft.access_token("sharepoint")
        self.assertEqual(len(FakePlatform.requests), 2)

    def test_the_azure_credential_has_the_shape_microsoft_calls(self):
        self._issue(60)
        token = toft.azure_credential("sharepoint").get_token(
            "https://graph.microsoft.com/.default")
        value, expires_on = token
        self.assertEqual(value, "eyJ.tok")
        self.assertEqual(token.token, "eyJ.tok")
        self.assertIsInstance(token.expires_on, int)

    def test_a_refusal_carries_the_platforms_words(self):
        self.route("POST /api/v1/app/tokens/sharepoint", 403, {
            "error": "this SharePoint connection does not hand out access tokens",
            "code": "sharepoint_tokens_off"})
        with self.assertRaises(ToftError) as ctx:
            toft.access_token("sharepoint")
        self.assertIn("does not hand out access tokens", str(ctx.exception))


class _Elsewhere(BaseHTTPRequestHandler):
    """A second host, which a redirect from the platform names."""

    seen: list = []

    def log_message(self, *args):
        pass

    def do_GET(self):
        _Elsewhere.seen.append(dict(self.headers))
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"{}")

    do_POST = do_GET


class TestOrdinaryCallsRefuseRedirects(PlatformTestCase):
    """urllib's default opener follows a 3xx and copies every header onto it.

    The app token and the act-for blob would then reach whatever host the
    Location names. An ordinary call refuses a redirect, as the Go SDK does;
    only ``follow`` has one to deal with, by hand.
    """

    def setUp(self):
        super().setUp()
        _Elsewhere.seen = []
        self.elsewhere = HTTPServer(("127.0.0.1", 0), _Elsewhere)
        threading.Thread(target=self.elsewhere.serve_forever, daemon=True).start()
        host, port = self.elsewhere.server_address
        self.target = f"http://{host}:{port}/steal"

    def tearDown(self):
        self.elsewhere.shutdown()
        self.elsewhere.server_close()
        super().tearDown()

    def test_the_app_token_does_not_follow_a_redirect(self):
        self.route("GET /api/v1/app/sends/s1", 302, {}, {"Location": self.target})
        with self.assertRaises(ToftError) as ctx:
            toft.notify.status("s1")
        self.assertIn("redirect", str(ctx.exception))
        self.assertEqual(_Elsewhere.seen, [], "the redirect was followed")

    def test_the_run_token_does_not_follow_a_redirect(self):
        from toft.client import Run
        self.route("POST /api/v1/runner/progress", 307, {}, {"Location": self.target})
        with self.assertRaises(ToftError):
            Run(self.base, "run-token", "r").progress(10)
        self.assertEqual(_Elsewhere.seen, [], "the redirect was followed")


class TestCallerSuppliedSegmentsAreEscaped(PlatformTestCase):
    """A kind or an id is one path segment, never a route or a query of its own."""

    def test_kinds_and_ids_are_quoted(self):
        self.route("GET /api/v1/app/sends/a%2F..%2Fdata%3Fx%3D1", 200, {"id": "x", "status": "sent"})
        toft.notify.status("a/../data?x=1")
        self.assertEqual(self.last()["path"], "/api/v1/app/sends/a%2F..%2Fdata%3Fx%3D1")

        with self.assertRaises(ToftError):
            toft.notify.send("slack?x=1", "hi")
        self.assertEqual(self.last()["path"], "/api/v1/app/notify/slack%3Fx%3D1")

        from toft.tokens import _reset
        _reset()
        with self.assertRaises(ToftError):
            toft.access_token("sharepoint/../../data")
        self.assertEqual(self.last()["path"], "/api/v1/app/tokens/sharepoint%2F..%2F..%2Fdata")


class TestDeliverSendsTheNameItWasGiven(PlatformTestCase):
    def test_a_name_with_a_separator_reaches_the_platform_unchanged(self):
        """The platform refuses a separator by name; the SDK must not hide it."""
        self.route("POST /api/v1/app/deliver/sharepoint", 400,
                   {"error": "the name contains '/'", "code": "bad_name"})
        with self.assertRaises(ToftError):
            toft.notify.deliver("sharepoint", ("report.xlsx", b"x"), name="2026/Week 32.xlsx")
        body = self.last()["body"]
        self.assertIn(b'name="name"\r\n\r\n2026/Week 32.xlsx\r\n', body)


class TestOffPlatform(unittest.TestCase):
    """No environment, no fake server: what a developer's laptop sees."""

    def setUp(self):
        session.reset(None)
        toft.app.refresh()
        for name in ("TOFT_API_URL", "TOFT_APP_TOKEN"):
            os.environ.pop(name, None)

    tearDown = setUp

    def test_importing_never_raises_and_never_calls_out(self):
        import importlib

        importlib.reload(toft)  # the real assertion is that this does not raise

    def test_a_send_names_the_missing_variable(self):
        with self.assertRaises(ToftError) as caught:
            toft.notify.slack("hello")
        self.assertIn("TOFT_API_URL", str(caught.exception))


class TestInsideAJob(unittest.TestCase):
    """A job is on the platform, and is deliberately given no app token.

    The generic refusal told an author whose job had just done its real work
    that they were not running on the platform, which sent them looking for a
    broken deploy. The absence is architectural and the sentence has to say so.
    """

    def setUp(self):
        session.reset(None)
        toft.app.refresh()
        os.environ["TOFT_API_URL"] = "http://platform.invalid"
        os.environ["TOFT_RUN_ID"] = "80ced99a"
        os.environ["TOFT_JOB_NAME"] = "archive"
        os.environ.pop("TOFT_APP_TOKEN", None)

    def tearDown(self):
        session.reset(None)
        toft.app.refresh()
        for name in ("TOFT_API_URL", "TOFT_RUN_ID", "TOFT_JOB_NAME"):
            os.environ.pop(name, None)

    def test_the_refusal_names_the_run_and_the_way_round_it(self):
        with self.assertRaises(ToftError) as caught:
            toft.data.get()
        message = str(caught.exception)
        self.assertIn("archive", message)
        self.assertIn("80ced99a", message)
        self.assertIn("deliberately", message)
        self.assertIn("write_output_json", message)
        self.assertNotIn("only works in an app running on the platform", message)

    def test_off_platform_still_gets_the_off_platform_sentence(self):
        os.environ.pop("TOFT_RUN_ID")
        with self.assertRaises(ToftError) as caught:
            toft.data.get()
        self.assertIn("only works in an app running on the platform", str(caught.exception))


class TestRunInsideAJob(unittest.TestCase):
    """What a job reads about itself, and what it reports.

    Params are the only input now, so the env the runner exports is the whole of
    what a job is given — and the deadline and rerun_of are new in it.
    """

    ENV = {
        "TOFT_JOB_NAME": "nightly-report",
        "TOFT_PARAMS": '{"region": "emea", "days": 7}',
        "TOFT_RERUN_OF": "run_0",
        "TOFT_DEADLINE": "2026-10-22T03:45:00.123456789Z",
        "TOFT_SCHEDULED_FOR": "2026-10-22T03:00:00+01:00",
    }

    def setUp(self):
        self.saved = {k: os.environ.get(k) for k in self.ENV}
        os.environ.update(self.ENV)

    def tearDown(self):
        for k, v in self.saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    def run_(self):
        return toft.Run("http://platform.invalid", "job-token", "run-1")

    def test_params_are_typed_and_the_new_facts_are_read(self):
        import datetime

        r = self.run_()
        self.assertEqual(r.job, "nightly-report")
        self.assertEqual(r.params, {"region": "emea", "days": 7})
        self.assertEqual(r.rerun_of, "run_0")
        # Go writes nine fractional digits and a Z; 3.9's fromisoformat takes neither.
        self.assertEqual(r.deadline, datetime.datetime(2026, 10, 22, 3, 45, 0, 123456,
                                                       tzinfo=datetime.timezone.utc))
        self.assertEqual(r.scheduled_for.utcoffset(), datetime.timedelta(hours=1))

    def test_absent_facts_are_none(self):
        for k in ("TOFT_RERUN_OF", "TOFT_DEADLINE"):
            os.environ.pop(k)
        r = self.run_()
        self.assertIsNone(r.rerun_of)
        self.assertIsNone(r.deadline)

    def test_the_input_channels_are_gone(self):
        r = self.run_()
        for name in ("set_result", "read_input", "read_input_json", "input_uri"):
            self.assertFalse(hasattr(r, name), name)

    def test_log_prints_to_the_stream_the_runner_ships(self):
        import contextlib
        import io

        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            self.run_().log("12,044 rows")
            self.run_().log("ledger missing", error=True)
        self.assertEqual(out.getvalue(), "12,044 rows\n")
        self.assertEqual(err.getvalue(), "ledger missing\n")


class TestStorage(unittest.TestCase):
    def tearDown(self):
        os.environ.pop("TOFT_STORAGE", None)

    def test_resolution_is_by_name(self):
        os.environ["TOFT_STORAGE"] = json.dumps(
            [{"name": "reports", "kind": "volume", "path": "/data/r-7", "access": "write"}]
        )
        mount = toft.storage("reports")
        self.assertEqual(str(mount.path), "/data/r-7")
        self.assertFalse(mount.read_only)

    def test_a_miss_names_what_is_mounted(self):
        os.environ["TOFT_STORAGE"] = json.dumps(
            [{"name": "reports", "kind": "volume", "path": "/a", "access": "full"},
             {"name": "inbox", "kind": "volume", "path": "/b", "access": "full"}]
        )
        with self.assertRaises(ToftError) as caught:
            toft.storage("report")  # a typo, the common case
        self.assertIn("inbox, reports", str(caught.exception))

    def test_no_storage_at_all_says_so(self):
        with mock.patch.dict(os.environ, {"TOFT_API_URL": "http://127.0.0.1:1"}):
            with self.assertRaises(ToftError) as caught:
                toft.storage("reports")
        self.assertIn("none mapped", str(caught.exception))
        self.assertEqual(caught.exception.code, "not_mapped")

    def test_off_platform_storage_is_off_platform(self):
        # BB third list, D-13: as Node's, not not_mapped.
        for var in ("TOFT_API_URL", "TOFT_APP_ID", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR"):
            os.environ.pop(var, None)
        for call in (toft.storage, toft.store):
            with self.assertRaises(ToftError) as caught:
                call("reports")
            self.assertEqual(caught.exception.code, "off_platform")

    def test_mounts_is_empty_rather_than_an_error(self):
        self.assertEqual(toft.mounts(), [])

    # One resource, mapped twice. A mapping's identity is (resource, app,
    # folder), so this is a shape an administrator can make on purpose — a read
    # corpus and a write drop, at two paths in one workload — and both arrive
    # under one name.
    TWO = json.dumps(
        [
            {"name": "drop", "kind": "store", "path": "/data/backup",
             "access": "write", "folder": "backup"},
            {"name": "drop", "kind": "store", "path": "/data/drop", "access": "read"},
        ]
    )

    def test_the_folder_reaches_app_code(self):
        os.environ["TOFT_STORAGE"] = self.TWO
        by_folder = {m.folder: m for m in toft.mounts()}
        self.assertEqual(set(by_folder), {"", "backup"})
        self.assertEqual(str(by_folder["backup"].path), "/data/backup")
        self.assertEqual(by_folder["backup"].access, "write")

    def test_an_ambiguous_name_is_refused_rather_than_resolved(self):
        """The defect this replaces answered, and answered by path order.

        Returning the first match made the choice by the alphabetical order of
        paths an administrator chose, which is the coupling TOFT_STORAGE
        exists to prevent — so the refusal has to name both folders *and* both
        levels, since the level is usually why there are two.
        """
        os.environ["TOFT_STORAGE"] = self.TWO
        with self.assertRaises(ToftError) as caught:
            toft.storage("drop")
        message = str(caught.exception)
        self.assertIn("folder 'backup' at write", message)
        self.assertIn("folder '' at read", message)
        self.assertIn("folder=", message)

    def test_naming_the_folder_answers(self):
        os.environ["TOFT_STORAGE"] = self.TWO
        self.assertEqual(str(toft.storage("drop", folder="backup").path), "/data/backup")
        self.assertEqual(str(toft.storage("drop", folder="").path), "/data/drop")
        self.assertEqual(str(toft.store("drop", folder="backup").path), "/data/backup")

    def test_a_folder_that_is_not_mounted_names_the_ones_that_are(self):
        os.environ["TOFT_STORAGE"] = self.TWO
        with self.assertRaises(ToftError) as caught:
            toft.storage("drop", folder="archive")
        self.assertIn("not at folder 'archive'", str(caught.exception))
        self.assertIn("folder 'backup' at write", str(caught.exception))

    def test_an_install_that_names_no_folder_is_unchanged(self):
        """The old-install case: every mount has folder "", so nothing is ambiguous."""
        os.environ["TOFT_STORAGE"] = json.dumps(
            [{"name": "reports", "kind": "volume", "path": "/data/r", "access": "full"}]
        )
        mount = toft.storage("reports")
        self.assertEqual(mount.folder, "")
        self.assertEqual(str(mount.path), "/data/r")


class TestSessionScratch(unittest.TestCase):
    """Two viewers, two directories — including when neither signed in.

    The failure this guards is a data leak rather than a crash: an app that
    answers a missing ``X-Toft-Session`` with a constant puts every
    anonymous viewer of a public app in one directory, where one viewer's
    upload overwrites another's. ``new_session_id`` is the answer, so what
    matters is that two mints never meet.
    """

    def setUp(self):
        root = tempfile.mkdtemp()
        os.environ["TOFT_SCRATCH_DIR"] = root
        self.root = Path(root)
        self.addCleanup(shutil.rmtree, root, True)
        self.addCleanup(os.environ.pop, "TOFT_SCRATCH_DIR", None)

    def test_every_mint_is_distinct_and_wide(self):
        ids = {toft.new_session_id() for _ in range(500)}
        self.assertEqual(len(ids), 500)
        # Guessable ids are as bad as a shared one: the directory name is a
        # hash of this string, so guessing it names somebody else's directory.
        self.assertGreaterEqual(min(len(i) for i in ids), 20)

    def test_two_anonymous_viewers_do_not_collide(self):
        first = toft.session_scratch_dir(toft.new_session_id())
        second = toft.session_scratch_dir(toft.new_session_id())
        self.assertNotEqual(first, second)
        self.assertTrue(first.is_dir() and second.is_dir())

    def test_one_viewer_carrying_an_id_back_returns_to_it(self):
        sid = toft.new_session_id()
        (toft.session_scratch_dir(sid) / "upload.csv").write_text("a,b\n")
        self.assertEqual((toft.session_scratch_dir(sid) / "upload.csv").read_text(), "a,b\n")

    def test_an_empty_id_is_refused_rather_than_collapsed(self):
        with self.assertRaises(toft.ToftError):
            toft.session_scratch_dir("")

    def test_a_hostile_id_cannot_traverse_out(self):
        self.assertEqual(toft.session_scratch_dir("../../etc").parent, self.root)


class TestMultipartEncoding(unittest.TestCase):
    def test_a_hostile_filename_cannot_break_out_of_the_header(self):
        part = Part("file", 'evil"\r\nX-Injected: yes', b"x")
        body, content_type = encode_multipart({}, [part])
        self.assertNotIn(b"X-Injected: yes\r\n", body.split(b"\r\n\r\n")[0])
        self.assertIn(b"boundary=", content_type.encode())

    def test_a_path_is_reduced_to_its_basename(self):
        part = Part("file", "/home/someone/private/report.xlsx", b"x")
        self.assertEqual(part.filename, "report.xlsx")

    def test_an_unusable_attachment_is_a_clear_error(self):
        with self.assertRaises(ToftError) as caught:
            Part.coerce("attachment", 42)
        self.assertIn("must be a path", str(caught.exception))


if __name__ == "__main__":
    unittest.main()


class TestJobs(PlatformTestCase):
    """Starting a different entrypoint of this same app.

    Every scenario here is one the Node SDK must reproduce: same routes, same
    headers, same reported deduplication, same typed refusal. An app written in
    one language must not be able to observe a platform the other cannot.
    """

    ACT_FOR = "signed-blob-for-priya"

    def viewer(self):
        return toft.viewer({"X-Toft-Act-For": self.ACT_FOR})

    def test_the_app_starts_a_run_as_itself(self):
        self.route(
            "POST /api/v1/app/jobs/reconcile/runs",
            202,
            {"id": "run_1", "status": "queued", "job": "reconcile", "started_by": "app",
             "queue_position": 2, "params": {"month": "2026-03"},
             "status_url": "/apps/books/__toft/runs/run_1"},
        )
        run = toft.jobs.start("reconcile", params={"month": "2026-03"})

        self.assertEqual(run.id, "run_1")
        self.assertEqual(run.job, "reconcile")
        self.assertEqual(run.queue_position, 2)
        self.assertEqual(run.params, {"month": "2026-03"})
        self.assertEqual(run.status_url, "/apps/books/__toft/runs/run_1")
        self.assertFalse(run.done)
        self.assertFalse(run.started_by_viewer)
        # No act-for header: the app is acting as itself, deliberately, and
        # nothing about that call may look like a person did it.
        self.assertNotIn("X-Toft-Act-For", self.last()["headers"])
        self.assertEqual(json.loads(self.last()["body"]), {"params": {"month": "2026-03"}})

    def test_a_viewer_start_relays_the_header_verbatim(self):
        self.route(
            "POST /api/v1/app/jobs/reconcile/runs",
            202,
            {"id": "run_2", "status": "queued", "started_by": "viewer"},
        )
        run = self.viewer().jobs.start("reconcile", params={"month": "2026-03"})

        self.assertTrue(run.started_by_viewer)
        self.assertEqual(self.last()["headers"].get("X-Toft-Act-For"), self.ACT_FOR)
        # The contract version rides on every app call, as it does everywhere
        # else on this surface.
        self.assertEqual(
            self.last()["headers"].get("X-Toft-App-Contract"), APP_CONTRACT_VERSION
        )

    def test_viewer_without_the_header_raises_rather_than_downgrading(self):
        # A silent fall back to the app acting as itself would move a run out of
        # a person's name and into nobody's, and nobody would notice until an
        # audit.
        with self.assertRaises(ToftError) as caught:
            toft.viewer({"X-Toft-Session": "abc"})
        self.assertIn("X-Toft-Act-For", str(caught.exception))
        self.assertEqual(len(FakePlatform.requests), 0)

    def test_a_deduplicated_start_is_reported_not_silent(self):
        self.route(
            "POST /api/v1/app/jobs/reconcile/runs",
            200,
            {"id": "run_1", "status": "running", "deduplicated": True, "progress": 40},
        )
        run = self.viewer().jobs.start("reconcile")

        self.assertTrue(run.deduplicated)
        self.assertEqual(run.id, "run_1")
        self.assertEqual(run.progress, 40)

    def test_an_explicit_idempotency_key_is_sent(self):
        self.route("POST /api/v1/app/jobs/reconcile/runs", 202, {"id": "run_3"})
        self.viewer().jobs.start("reconcile", idempotency_key="mine")
        self.assertEqual(self.last()["headers"].get("Idempotency-Key"), "mine")

    def test_a_full_queue_is_a_typed_refusal_carrying_the_numbers(self):
        self.route(
            "POST /api/v1/app/jobs/reconcile/runs",
            429,
            {"error": "too many runs already in flight", "code": "queue_full",
             "in_flight": 3, "limit": 3, "retryable": True},
            {"Retry-After": "10"},
        )
        with self.assertRaises(toft.QueueFull) as caught:
            toft.jobs.start("reconcile")

        exc = caught.exception
        self.assertEqual((exc.in_flight, exc.limit), (3, 3))
        self.assertTrue(exc.retryable)
        self.assertEqual(exc.retry_after, 10)
        # The envelope survives the promotion; it read code "" on trytoft.dev.
        self.assertEqual(exc.code, "queue_full")

    def test_mine_scopes_to_the_viewer_and_needs_one(self):
        self.route(
            "GET /api/v1/app/job-runs",
            200,
            {"runs": [{"id": "run_1", "status": "succeeded", "started_by": "viewer"}]},
        )
        runs = self.viewer().jobs.mine(job="reconcile", limit=20)

        self.assertEqual([r.id for r in runs], ["run_1"])
        self.assertIn("limit=20", self.last()["path"])
        self.assertTrue(runs[0].succeeded)
        self.assertIn("scope=viewer", self.last()["path"])
        self.assertIn("job=reconcile", self.last()["path"])
        self.assertEqual(self.last()["headers"].get("X-Toft-Act-For"), self.ACT_FOR)

        # The app acting as itself has no "mine" to ask about, and answering
        # with everything would be a silent widening rather than an answer.
        before = len(FakePlatform.requests)
        with self.assertRaises(ToftError) as caught:
            toft.jobs.mine()
        self.assertEqual(len(FakePlatform.requests), before)
        # A refusal no retry can change says so, with a code to branch on.
        self.assertEqual(caught.exception.code, "no_viewer")
        self.assertFalse(caught.exception.retryable)
        self.assertIn("toft.viewer", caught.exception.hint)

    def test_list_asks_for_the_app_scope(self):
        self.route("GET /api/v1/app/job-runs", 200, {"runs": []})
        self.assertEqual(toft.jobs.list(), [])
        self.assertIn("scope=app", self.last()["path"])
        self.assertNotIn("status=", self.last()["path"])

    def test_list_filters_on_job_and_status(self):
        self.route("GET /api/v1/app/job-runs", 200,
                   {"runs": [{"id": "run_9", "job": "nightly", "status": "failed",
                              "failure_reason": "exit 3: connection refused"}]})
        runs = toft.jobs.list(job="nightly", status="failed", limit=5)
        path = self.last()["path"]
        for part in ("job=nightly", "status=failed", "limit=5"):
            self.assertIn(part, path)
        self.assertEqual(runs[0].failure_reason, "exit 3: connection refused")

    def test_start_sends_params_alone(self):
        # payload is gone: params are the only input, and an unknown keyword is
        # a TypeError rather than something quietly dropped on the floor.
        self.route("POST /api/v1/app/jobs/reconcile/runs", 202, {"id": "run_1"})
        with self.assertRaises(TypeError):
            toft.jobs.start("reconcile", payload={"rows": [1]})
        toft.jobs.start("reconcile")
        self.assertEqual(json.loads(self.last()["body"]), {})

    def test_a_run_carries_every_contract_field(self):
        from toft.jobs import JobRun

        run = JobRun.from_raw({
            "id": "r", "job": "j", "status": "out_of_memory", "started_by": "schedule",
            "rerun_of": "r0", "params": {"a": 1}, "scheduled_for": "2026-10-22T03:00:00Z",
            "queued_at": "q", "started_at": "s", "finished_at": "f", "deadline": "d",
            "progress": 80, "progress_message": "writing", "has_output": True,
            "exit_code": 137, "failure_stage": "run", "failure_reason": "stopped at 1024 MB",
            "peak_memory_mb": 1024, "status_url": "/apps/a/__toft/runs/r",
            "logs_url": "/api/v1/app/job-runs/r/logs",
        })
        self.assertEqual(
            (run.rerun_of, run.deadline, run.progress_message, run.peak_memory_mb, run.logs_url),
            ("r0", "d", "writing", 1024, "/api/v1/app/job-runs/r/logs"),
        )
        self.assertTrue(run.done)
        self.assertTrue(run.out_of_resources)

    def test_run_status_words_match_the_platforms(self):
        cases = {
            "succeeded": ("done", "succeeded"),
            "failed": ("done",),
            "timed_out": ("done", "out_of_resources"),
            "out_of_memory": ("done", "out_of_resources"),
            "cancelled": ("done",),
            "skipped": ("done",),
            "running": (),
            "queued": (),
        }
        from toft.jobs import JobRun

        for status, expected in cases.items():
            run = JobRun.from_raw({"id": "r", "status": status})
            self.assertEqual(run.done, "done" in expected, status)
            self.assertEqual(run.succeeded, "succeeded" in expected, status)
            self.assertEqual(
                run.out_of_resources, "out_of_resources" in expected, status
            )

    def test_cancel_and_logs_go_to_the_run(self):
        self.route("POST /api/v1/app/job-runs/run_1/cancel", 204, {})
        self.route(
            "GET /api/v1/app/job-runs/run_1/logs",
            200,
            {"lines": [{"seq": 1, "line": "reconciling"}], "status": "running"},
        )
        from toft.jobs import JobRun

        run = JobRun.from_raw({"id": "run_1"}, {"X-Toft-Act-For": self.ACT_FOR})
        self.assertEqual(run.logs()[0]["line"], "reconciling")
        self.assertIn("after=0", self.last()["path"])

        run.cancel()
        # The viewer rides along, because who may cancel a run is decided by
        # who started it.
        self.assertEqual(self.last()["headers"].get("X-Toft-Act-For"), self.ACT_FOR)

    def test_output_returns_none_when_a_run_produced_none(self):
        self.route("GET /api/v1/app/job-runs/run_1/output", 404,
                   {"error": "this run produced no output object", "code": "no_output"})
        from toft.jobs import JobRun

        self.assertIsNone(JobRun.from_raw({"id": "run_1"}).output())
        self.assertIsNone(JobRun.from_raw({"id": "run_1"}).output_json())

    def test_output_json_decodes_what_the_job_wrote(self):
        self.route("GET /api/v1/app/job-runs/run_1/output", 200, {"rows": 1204})
        from toft.jobs import JobRun

        self.assertEqual(JobRun.from_raw({"id": "run_1"}).output_json(), {"rows": 1204})

    def test_a_listing_iterates_its_objects(self):
        from toft.resources import Listing

        listing = Listing(objects=[{"name": "a.csv"}, {"name": "b.csv"}], prefixes=["backup/"])
        self.assertEqual([o["name"] for o in listing], ["a.csv", "b.csv"])
        self.assertEqual(len(listing), 2)

    def test_output_raises_when_the_artifact_expired(self):
        # Distinct from "produced none": a run still reading succeeded with
        # nothing behind it is the failure this distinction exists to prevent,
        # and it must not read as an empty report.
        self.route("GET /api/v1/app/job-runs/run_1/output", 410,
                   {"error": "this run's output has expired", "code": "output_expired"})
        from toft.jobs import JobRun

        with self.assertRaises(ToftError) as caught:
            JobRun.from_raw({"id": "run_1"}).output()
        self.assertEqual(caught.exception.code, "output_expired")


class TestNoRetries(PlatformTestCase):
    """A run is never retried, so nothing here follows a chain.

    A field an older platform might still send (``retried_by``) must not make a
    failed run read as unfinished, and refresh() must not chase it.
    """

    def test_a_failed_run_is_done_and_refresh_reads_only_itself(self):
        from toft.jobs import JobRun

        self.route("GET /api/v1/app/job-runs/run_1", 200,
                   {"id": "run_1", "status": "failed", "retried_by": "run_2"})
        self.route("GET /api/v1/app/job-runs/run_2", 200, {"id": "run_2", "status": "succeeded"})

        run = JobRun.from_raw({"id": "run_1", "status": "failed", "retried_by": "run_2"})
        self.assertTrue(run.done)
        again = run.refresh()
        self.assertEqual(again.id, "run_1")
        self.assertFalse(again.succeeded)
        self.assertEqual([r["path"] for r in FakePlatform.requests], ["/api/v1/app/job-runs/run_1"])

    def test_wait_returns_the_run_in_its_final_state(self):
        from toft.jobs import JobRun

        self.script("GET /api/v1/app/job-runs/run_1", [
            (200, {"id": "run_1", "status": "running", "progress": 50}, None),
            (200, {"id": "run_1", "status": "succeeded", "progress": 100, "has_output": True}, None),
        ])
        final = JobRun.from_raw({"id": "run_1"}).wait(poll_interval=0.01)
        self.assertTrue(final.succeeded)
        self.assertTrue(final.has_output)

    def test_wait_waits_out_the_rate_limit_rather_than_raising(self):
        # A busy app's other calls spend the same limiter; a poll it refuses
        # is not the run failing (jobs-regression #4).
        from toft.jobs import JobRun

        limited = (429, {"code": "rate_limited", "retryable": True, "error": "slow down"},
                   {"Retry-After": "0"})
        self.script("GET /api/v1/app/job-runs/run_1", [
            (200, {"id": "run_1", "status": "running"}, None),
            limited, limited, limited,
            (200, {"id": "run_1", "status": "succeeded"}, None),
        ])
        final = JobRun.from_raw({"id": "run_1"}).wait(poll_interval=0.01)
        self.assertTrue(final.succeeded)

    def test_a_wait_that_runs_out_is_coded_and_still_a_timeout(self):
        from toft.jobs import JobRun

        self.route("GET /api/v1/app/job-runs/run_1", 200, {"id": "run_1", "status": "running"})
        with self.assertRaises(TimeoutError) as caught:
            JobRun.from_raw({"id": "run_1"}).wait(timeout=0, poll_interval=0.01)
        self.assertEqual(caught.exception.code, "wait_timeout")
        self.assertTrue(caught.exception.retryable)


class TestRespond(unittest.TestCase):
    """respond(run): 202 + Location + the run for a script, 303 for a browser."""

    RUN = {"id": "run_1", "job": "nightly", "status": "queued",
           "status_url": "/apps/books/__toft/runs/run_1"}

    def run_(self):
        from toft.jobs import JobRun

        return JobRun.from_raw(self.RUN)

    def test_a_script_gets_202_location_and_the_run(self):
        body, status, headers = toft.jobs.respond(self.run_(), request={"Accept": "application/json"})
        self.assertEqual(status, 202)
        self.assertEqual(headers["Location"], self.RUN["status_url"])
        self.assertEqual(headers["Content-Type"], "application/json")
        # The run as sent, with the fields the platform leaves out when empty
        # present as null (BB-030, fourth 2026-11-03 black-box list).
        self.assertEqual(json.loads(body), {
            **self.RUN, "rerun_of": None, "scheduled_for": None, "deadline": None,
            "exit_code": None, "failure_reason": None, "deduplicated": False})

    def test_no_request_is_the_json_answer(self):
        _, status, _ = toft.jobs.respond(self.run_())
        self.assertEqual(status, 202)

    def test_a_browser_gets_303_to_the_status_page(self):
        accept = "text/html,application/xhtml+xml,*/*;q=0.8"
        body, status, headers = toft.jobs.respond(self.run_(), request={"accept": accept})
        self.assertEqual((body, status), ("", 303))
        self.assertEqual(headers["Location"], self.RUN["status_url"])

    def test_flask_gets_a_flask_response_from_its_own_request(self):
        import types

        made = {}

        class FakeResponse:
            def __init__(self, body, status=None, headers=None, mimetype=None):
                made.update(body=body, status=status, headers=headers, mimetype=mimetype)

        flask = types.ModuleType("flask")
        flask.has_request_context = lambda: True
        flask.request = types.SimpleNamespace(headers={"Accept": "text/html"})
        flask.Response = FakeResponse
        sys.modules["flask"] = flask
        try:
            self.assertIsInstance(toft.jobs.respond(self.run_()), FakeResponse)
        finally:
            del sys.modules["flask"]
        self.assertEqual(made["status"], 303)
        self.assertEqual(made["headers"], {"Location": self.RUN["status_url"]})

    def test_starlette_gets_a_starlette_response_when_given_the_request(self):
        import types

        made = {}

        class FakeResponse:
            def __init__(self, content=None, status_code=200, headers=None, media_type=None):
                made.update(content=content, status=status_code, headers=headers, media=media_type)

        responses = types.ModuleType("starlette.responses")
        responses.Response = FakeResponse
        sys.modules["starlette"] = types.ModuleType("starlette")
        sys.modules["starlette.responses"] = responses
        Request = type("Request", (), {"__module__": "starlette.requests"})
        request = Request()
        request.headers = {"accept": "application/json"}
        try:
            self.assertIsInstance(toft.jobs.respond(self.run_(), request=request), FakeResponse)
        finally:
            del sys.modules["starlette.responses"], sys.modules["starlette"]
        self.assertEqual((made["status"], made["media"]), (202, "application/json"))
        self.assertEqual(json.loads(made["content"])["id"], "run_1")


class TestWaitInsideARequest(PlatformTestCase):
    """wait() on a request path refuses rather than ending as a 504."""

    ACT_FOR = "signed-blob"

    def deadline_in(self, seconds):
        import datetime

        at = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=seconds)
        return at.strftime("%Y-%m-%dT%H:%M:%SZ")

    def started(self, headers):
        self.route("POST /api/v1/app/jobs/nightly/runs", 202, {"id": "run_1", "status": "queued"})
        self.route("GET /api/v1/app/job-runs/run_1", 200, {"id": "run_1", "status": "succeeded"})
        return toft.viewer(headers).jobs.start("nightly")

    def assertOutlives(self, fn):
        before = len(FakePlatform.requests)
        with self.assertRaises(ToftError) as caught:
            fn()
        err = caught.exception
        self.assertEqual(err.code, "would_outlive_request")
        self.assertFalse(err.retryable)
        self.assertEqual(err.hint, "return toft.jobs.respond(run)")
        # Refused at once: not one poll was spent finding out.
        self.assertEqual(len(FakePlatform.requests), before)

    def test_a_viewer_run_with_no_timeout_is_refused(self):
        run = self.started({"X-Toft-Act-For": self.ACT_FOR,
                            "X-Toft-Request-Deadline": self.deadline_in(300)})
        self.assertOutlives(run.wait)

    def test_a_timeout_past_the_deadline_is_refused(self):
        run = self.started({"X-Toft-Act-For": self.ACT_FOR,
                            "X-Toft-Request-Deadline": self.deadline_in(30)})
        self.assertOutlives(lambda: run.wait(timeout=60))

    def test_a_timeout_that_fits_is_allowed(self):
        run = self.started({"X-Toft-Act-For": self.ACT_FOR,
                            "X-Toft-Request-Deadline": self.deadline_in(300)})
        self.assertTrue(run.wait(timeout=10).succeeded)
        # And the refreshed handle still knows the request it belongs to —
        # read while still going, since a finished run's wait returns at once.
        self.route("GET /api/v1/app/job-runs/run_1", 200, {"id": "run_1", "status": "running"})
        self.assertOutlives(run.refresh().wait)

    def test_a_finished_run_returns_at_once_with_no_timeout(self):
        # BB-072: nothing to outlive, so no would_outlive_request.
        run = self.started({"X-Toft-Act-For": self.ACT_FOR,
                            "X-Toft-Request-Deadline": self.deadline_in(300)}).refresh()
        before = len(FakePlatform.requests)
        self.assertTrue(run.wait().succeeded)
        self.assertEqual(len(FakePlatform.requests), before)

    def test_no_deadline_is_a_script_and_waits_as_before(self):
        run = self.started({"X-Toft-Act-For": self.ACT_FOR})
        self.assertTrue(run.wait().succeeded)

    def test_explicit_headers_guard_an_app_run(self):
        from toft.jobs import JobRun

        run = JobRun.from_raw({"id": "run_1"})
        self.assertOutlives(lambda: run.wait(headers={"x-toft-request-deadline": self.deadline_in(300)}))

    def test_a_websocket_handshake_carries_no_deadline(self):
        # Streamlit holds its handshake headers for the whole session; that
        # request's deadline passed hours ago and means nothing now.
        run = self.started({"X-Toft-Act-For": self.ACT_FOR, "Upgrade": "websocket",
                            "X-Toft-Request-Deadline": self.deadline_in(-3600)})
        self.assertTrue(run.wait(timeout=5).succeeded)

    def test_flasks_current_request_is_seen(self):
        import types
        from toft.jobs import JobRun

        flask = types.ModuleType("flask")
        flask.has_request_context = lambda: True
        flask.request = types.SimpleNamespace(
            headers={"X-Toft-Request-Deadline": self.deadline_in(300)})
        sys.modules["flask"] = flask
        try:
            self.assertOutlives(JobRun.from_raw({"id": "run_1"}).wait)
        finally:
            del sys.modules["flask"]


class TestAppData(PlatformTestCase):
    """The app's one JSON document.

    The same scenarios ``sdk/node/test/data.test.js`` runs — same route, same
    body shape, same precondition header, same typed conflict — so an app
    written in one language cannot observe a platform the other cannot.
    """

    def test_never_written_reads_as_none(self):
        self.route("GET /api/v1/app/data", 200,
                   {"data": None, "version": 0, "updated_at": None, "bytes": 0})
        self.assertIsNone(toft.data.get())
        self.assertTrue(toft.data.read().empty)

    def test_set_sends_the_document_as_the_body(self):
        self.route("PUT /api/v1/app/data", 200,
                   {"version": 1, "updated_at": "2026-08-14T09:00:00Z", "bytes": 21})
        saved = toft.data.set({"period": "2026-03"})

        req = self.last()
        # The body *is* the document — not a wrapper with it inside a field.
        self.assertEqual(json.loads(req["body"]), {"period": "2026-03"})
        self.assertNotIn("If-Match", req["headers"])
        self.assertEqual(saved.version, 1)
        self.assertEqual(saved.bytes, 21)

    def test_the_returned_document_carries_what_was_written(self):
        """The platform's reply to a PUT is metadata only.

        Without filling it in, ``doc = toft.data.set(...)`` came back with
        ``data=None`` — the value this API reserves for "never written" — for a
        document that had just been written and is not empty.
        """
        self.route("PUT /api/v1/app/data", 200, {"version": 2, "bytes": 21})
        saved = toft.data.set({"period": "2026-03"})
        self.assertEqual(saved.data, {"period": "2026-03"})
        self.assertFalse(saved.empty)

        self.script("GET /api/v1/app/data", [(200, {"data": {"runs": 1}, "version": 1}, None)])
        self.script("PUT /api/v1/app/data", [(200, {"version": 2, "bytes": 12}, None)])
        updated = toft.data.update(lambda s: s.update(runs=s.get("runs", 0) + 1))
        self.assertEqual(updated.data, {"runs": 2})

    def test_set_with_a_version_sends_the_precondition(self):
        self.route("PUT /api/v1/app/data", 200, {"version": 4, "bytes": 2})
        toft.data.set({"a": 1}, if_version=3)
        self.assertEqual(self.last()["headers"].get("If-Match"), '"3"')

    def test_a_conflict_is_typed_and_carries_the_stored_version(self):
        self.route("PUT /api/v1/app/data", 412,
                   {"error": "the document changed", "code": "version_conflict", "version": 9})
        with self.assertRaises(toft.VersionConflict) as caught:
            toft.data.set({"a": 1}, if_version=3)
        self.assertEqual(caught.exception.version, 9)

    def test_update_re_reads_and_re_applies_after_a_conflict(self):
        self.script("GET /api/v1/app/data", [
            (200, {"data": {"runs": 1}, "version": 1, "bytes": 12}, None),
            (200, {"data": {"runs": 5}, "version": 2, "bytes": 12}, None),
        ])
        self.script("PUT /api/v1/app/data", [
            (412, {"code": "version_conflict", "version": 2, "error": "changed"}, None),
            (200, {"version": 3, "bytes": 12}, None),
        ])

        out = toft.data.update(lambda s: s.update(runs=s.get("runs", 0) + 1))

        self.assertEqual(out.version, 3)
        puts = [r for r in FakePlatform.requests if r["method"] == "PUT"]
        self.assertEqual(len(puts), 2)
        # The second attempt is built on the *second* read: the other writer's
        # change survives instead of being overwritten.
        self.assertEqual(json.loads(puts[0]["body"]), {"runs": 2})
        self.assertEqual(puts[0]["headers"].get("If-Match"), '"1"')
        self.assertEqual(json.loads(puts[1]["body"]), {"runs": 6})
        self.assertEqual(puts[1]["headers"].get("If-Match"), '"2"')

    def test_update_waits_out_the_rate_limit_rather_than_losing_the_write(self):
        # Several writers at once spend the app's burst between them; the
        # update is the write that must not be lost to it.
        limited = (429, {"code": "rate_limited", "retryable": True, "error": "slow down"},
                   {"Retry-After": "0"})
        self.script("GET /api/v1/app/data", [
            (200, {"data": {"runs": 1}, "version": 1, "bytes": 12}, None),
        ])
        # More refusals than attempts: the waits do not spend them (rerun #3).
        self.script("PUT /api/v1/app/data", [
            limited, limited, limited,
            (200, {"version": 2, "bytes": 12}, None),
        ])

        out = toft.data.update(lambda s: s.update(runs=s.get("runs", 0) + 1), attempts=2)

        self.assertEqual(out.version, 2)

    def test_update_gives_up_rather_than_looping_forever(self):
        self.route("GET /api/v1/app/data", 200, {"data": {}, "version": 1})
        self.route("PUT /api/v1/app/data", 412,
                   {"code": "version_conflict", "version": 2, "error": "changed"})
        with self.assertRaises(ToftError) as raised:
            toft.data.update(lambda s: s.update(a=1), attempts=2)
        puts = [r for r in FakePlatform.requests if r["method"] == "PUT"]
        self.assertEqual(len(puts), 2)
        self.assertEqual(raised.exception.code, "version_conflict")

    def test_an_exhausted_limiter_is_raised_as_itself(self):
        # Past its waits, update raises the refusal that stopped it — naming the
        # limiter and carrying retry_after — not a sentence about contention.
        data_module = sys.modules[type(toft.data).__module__]
        self.route("GET /api/v1/app/data", 429,
                   {"code": "rate_limited", "retryable": True, "error": "slow down"},
                   {"Retry-After": "7"})
        original = data_module.RATE_LIMIT_TIMEOUT
        data_module.RATE_LIMIT_TIMEOUT = 0
        try:
            with self.assertRaises(ToftError) as raised:
                toft.data.update(lambda s: s.update(a=1))
        finally:
            data_module.RATE_LIMIT_TIMEOUT = original
        self.assertEqual(raised.exception.code, "rate_limited")
        self.assertEqual(raised.exception.retry_after, 7)

    def test_clear_deletes_and_reports_what_it_removed(self):
        self.route("DELETE /api/v1/app/data", 200, {"version": 0, "deleted": True})
        self.assertTrue(toft.data.clear())
        self.assertEqual(self.last()["method"], "DELETE")

    def test_a_non_object_is_refused_locally(self):
        # Not a duplicated platform limit: None here would be serialized as {}
        # and quietly wipe the app's settings.
        for value in (None, [1, 2], "hello", 3):
            with self.assertRaises(ToftError):
                toft.data.set(value)
        self.assertEqual(FakePlatform.requests, [])


class TestWho(PlatformTestCase):
    """Who is looking at this app.

    The same scenarios `sdk/node/test/viewer.test.js` runs, so an app written in
    one language cannot observe a platform the other cannot.
    """

    blob = "act-for-blob"

    def setUp(self):
        super().setUp()
        _reset_identity_cache()
        self.headers = {toft.ACT_FOR_HEADER: self.blob}
        self.identity = {
            "id": "u_1",
            "email": "alice@corp.example",
            "name": "Alice Chen",
            "role": "editor",
            "is_admin": False,
            "groups": [{"id": "g_1", "name": "finance"}, {"id": "g_2", "name": "nyc-office"}],
        }

    def test_returns_the_person_with_names_on_their_groups(self):
        self.route("GET /api/v1/app/viewer", 200, self.identity)

        me = toft.who(self.headers)
        self.assertEqual(me.id, "u_1")
        self.assertEqual(me.email, "alice@corp.example")
        self.assertEqual(me.role, "editor")
        self.assertFalse(me.is_admin)
        self.assertEqual(me.group_names, ("finance", "nyc-office"))
        # The blob is relayed, not the app's own claim about who is looking.
        self.assertEqual(self.last()["headers"].get(toft.ACT_FOR_HEADER), self.blob)

    def test_no_header_is_nobody_and_costs_no_request(self):
        self.assertIsNone(toft.who({}))
        self.assertEqual(len(FakePlatform.requests), 0)

    def test_204_is_nobody(self):
        self.route("GET /api/v1/app/viewer", 204, None)
        self.assertIsNone(toft.who(self.headers))

    def test_403_raises_and_names_the_setting(self):
        """The one that must never be mistaken for 204.

        204 is *there is no viewer*; 403 is *there is a viewer and this app may
        not be told who*. An app told "nobody" would render its signed-out state
        to a signed-in person, and its author would have no reason to suspect a
        setting rather than a bug in their own code.
        """
        self.route(
            "GET /api/v1/app/viewer",
            403,
            {
                "error": "viewer identity is not enabled for this app — an administrator "
                "can enable it",
                "code": "viewer_identity_disabled",
            },
        )
        with self.assertRaises(ToftError) as caught:
            toft.who(self.headers)
        self.assertEqual(caught.exception.code, "viewer_identity_disabled")
        self.assertIn("administrator", str(caught.exception))

    def test_email_is_none_when_the_platform_withheld_it(self):
        withheld = dict(self.identity)
        withheld.pop("email")
        self.route("GET /api/v1/app/viewer", 200, withheld)

        me = toft.who(self.headers)
        self.assertIsNone(me.email)
        # Everything else still answers in full.
        self.assertEqual(me.id, "u_1")
        self.assertEqual(me.role, "editor")

    def test_resolves_once_per_blob_within_the_ttl(self):
        self.route("GET /api/v1/app/viewer", 200, self.identity)

        toft.who(self.headers)
        toft.who(self.headers)
        toft.who(self.headers)

        self.assertEqual(
            len(FakePlatform.requests), 1, "a per-rerun call became a request per rerun"
        )

    def test_refresh_goes_back_to_the_platform(self):
        demoted = dict(self.identity, role="viewer")
        self.script(
            "GET /api/v1/app/viewer",
            [(200, self.identity, None), (200, demoted, None)],
        )

        self.assertEqual(toft.who(self.headers).role, "editor")
        self.assertEqual(toft.who(self.headers, refresh=True).role, "viewer")
        self.assertEqual(len(FakePlatform.requests), 2)

    def test_two_viewers_are_kept_apart(self):
        self.script(
            "GET /api/v1/app/viewer",
            [(200, self.identity, None), (200, dict(self.identity, id="u_2"), None)],
        )

        first = toft.who({toft.ACT_FOR_HEADER: "blob-a"})
        second = toft.who({toft.ACT_FOR_HEADER: "blob-b"})
        self.assertEqual(first.id, "u_1")
        self.assertEqual(second.id, "u_2")

    def test_viewer_still_raises_where_who_returns_none(self):
        """Two entrypoints, and which one raises is the difference.

        `viewer()` is *act as this person*: a silent downgrade would move a run
        out of somebody's name and into nobody's.
        """
        self.assertIsNone(toft.who({}))
        with self.assertRaises(ToftError):
            toft.viewer({})

    def test_identity_on_a_viewer_is_the_same_call(self):
        self.route("GET /api/v1/app/viewer", 200, self.identity)
        me = toft.viewer(self.headers)
        self.assertEqual(me.identity().id, "u_1")


class TestTailReadsEveryPage(PlatformTestCase):
    """Code review 2026-11-11, I-1: the logs route answers a thousand lines a
    page and says nothing about the rest, so tail() reads until an empty page
    rather than stopping two pages past the point it saw the run finish."""

    def test_tail_yields_past_the_first_thousand_lines(self):
        total = 2500
        self.route("GET /api/v1/app/job-runs/r1", 200, {"id": "r1", "job": "nightly", "status": "failed"})
        # One scripted answer per ?after page, in the order tail asks for them.
        pages = []
        for after in range(0, total, 1000):
            pages.append((200, {"lines": [{"seq": s, "stream": "stdout", "line": f"line {s}"}
                                          for s in range(after + 1, min(after + 1000, total) + 1)]}, None))
        pages.append((200, {"lines": []}, None))
        self.script("GET /api/v1/app/job-runs/r1/logs", pages)
        from toft.jobs import JobRun
        run = JobRun(id="r1", status="running")
        got = [line["seq"] for line in run.tail(poll_interval=0.001)]
        self.assertEqual(len(got), total)
        self.assertEqual(got[-1], total)
