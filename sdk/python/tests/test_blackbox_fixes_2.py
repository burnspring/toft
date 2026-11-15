"""Regressions for the second 2026-11-03 black-box report's Python findings
(BB-002, BB-018, BB-020, BB-022)."""

from __future__ import annotations

import sys
import time
import types
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft.caller import ENV_ROLE_KEY, ROLE_HEADER  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun, respond  # noqa: E402

from test_app_sdk import PlatformTestCase  # noqa: E402
from test_caller import KEY, mint  # noqa: E402


class TestOutputJSONNotJSON(unittest.TestCase):
    """BB-002: output that is not JSON is a coded not_json, not a JSONDecodeError."""

    def test_not_json_is_coded_and_final(self):
        run = JobRun.from_raw({"id": "run_1"})
        with mock.patch.object(JobRun, "output", return_value=b"not json"):
            with self.assertRaises(ToftError) as ctx:
                run.output_json()
        self.assertEqual(ctx.exception.code, "not_json")
        self.assertFalse(ctx.exception.retryable)
        self.assertEqual(ctx.exception.hint, "read the bytes with output()")

    def test_no_output_is_none(self):
        run = JobRun.from_raw({"id": "run_1"})
        with mock.patch.object(JobRun, "output", return_value=None):
            self.assertIsNone(run.output_json())


class TestAnonymousViewerRepr(unittest.TestCase):
    """BB-018: a Viewer built for an anonymous visitor says so."""

    def test_repr_follows_the_verified_role(self):
        with mock.patch.dict("os.environ", {ENV_ROLE_KEY: KEY}):
            exp = int(time.time()) + 60
            anon = toft.viewer({"X-Toft-Act-For": "blob", ROLE_HEADER: mint(KEY, "anonymous", exp)})
            self.assertEqual(repr(anon), "<toft.Viewer (an anonymous visitor; nobody is signed in)>")
            person = toft.viewer({"X-Toft-Act-For": "blob", ROLE_HEADER: mint(KEY, "viewer", exp)})
            self.assertEqual(repr(person), "<toft.Viewer (a signed-in person; identity() says who, where viewer identity is on)>")
            self.assertEqual(repr(toft.viewer({"X-Toft-Act-For": "blob"})),
                             "<toft.Viewer (this request's viewer; identity() says who, where viewer identity is on)>")


class TestRespondWithoutRequestUnderFastAPI(unittest.TestCase):
    """BB-020: respond() without request= in a FastAPI app refuses rather than
    returning a tuple FastAPI would send as a 200 JSON array."""

    def test_refused_when_fastapi_is_loaded(self):
        run = JobRun.from_raw({"id": "run_1", "status_url": "/__toft/runs/run_1"})
        with mock.patch.dict(sys.modules, {"fastapi": types.ModuleType("fastapi")}), \
                mock.patch("toft.jobs._flask_request", return_value=None):
            with self.assertRaises(ToftError) as ctx:
                respond(run)
        self.assertEqual(ctx.exception.code, "bad_request")
        self.assertIn("request=request", ctx.exception.hint)

    def test_streamlit_is_told_to_link_the_status_page(self):
        # drills 2026-11-03: Streamlit loads Starlette too, and was handed the
        # FastAPI hint — a request= it has no request to pass.
        run = JobRun.from_raw({"id": "run_1", "status_url": "/__toft/runs/run_1"})
        with mock.patch.dict(sys.modules, {"streamlit": types.ModuleType("streamlit"),
                                           "starlette": types.ModuleType("starlette")}), \
                mock.patch("toft.jobs._flask_request", return_value=None):
            with self.assertRaises(ToftError) as ctx:
                respond(run)
        self.assertEqual(ctx.exception.code, "bad_request")
        self.assertIn("status_url", ctx.exception.hint)
        self.assertNotIn("request=request", ctx.exception.hint)

    def test_plain_wsgi_still_gets_the_tuple(self):
        run = JobRun.from_raw({"id": "run_1", "status_url": "/__toft/runs/run_1"})
        mods = {k: v for k, v in sys.modules.items()
                if k.split(".")[0] not in ("fastapi", "starlette", "streamlit", "marimo")}
        with mock.patch.dict(sys.modules, mods, clear=True), \
                mock.patch("toft.jobs._flask_request", return_value=None):
            body, status, headers = respond(run)
        self.assertEqual(status, 202)


class TestMineAnonymousHint(PlatformTestCase):
    """BB-022: an anonymous mine() is told about listing, not starting."""

    def test_hint_is_about_listing(self):
        self.route("GET /api/v1/app/job-runs", 403, {
            "error": "this request came from a visitor who is not signed in",
            "code": "viewer_anonymous", "retryable": False,
            "hint": "start it as the app: call the job surface without the viewer"})
        v = toft.viewer({"X-Toft-Act-For": "blob"})
        with self.assertRaises(ToftError) as ctx:
            v.jobs.mine()
        self.assertEqual(ctx.exception.code, "viewer_anonymous")
        self.assertEqual(ctx.exception.status, 403)
        self.assertIn("toft.jobs.list", ctx.exception.hint)
        self.assertNotIn("start it", ctx.exception.hint)


if __name__ == "__main__":
    unittest.main()
