"""Regressions for the black-box report's SDK findings (BB-006, -007, -008,
-019, -034 and the input-validation list).

Every refusal here is one the SDK makes itself: coded, never retryable, and —
where it is about an argument — made before any request reaches the platform.
"""

from __future__ import annotations

import os
import shutil
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft import scratch as scratch_module  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun  # noqa: E402

from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402


class Refusals:
    def assertRefused(self, code, fn, *args, **kwargs):
        with self.assertRaises(ToftError) as ctx:
            fn(*args, **kwargs)
        self.assertEqual(ctx.exception.code, code, str(ctx.exception))
        self.assertFalse(ctx.exception.retryable, str(ctx.exception))
        return ctx.exception


class TestScratchStaysInside(Refusals, unittest.TestCase):
    """BB-006: a scratch part cannot name a path outside the scratch root."""

    def setUp(self):
        root = tempfile.mkdtemp()
        self.root = Path(root)
        self.addCleanup(shutil.rmtree, root, True)
        os.environ["TOFT_SCRATCH_DIR"] = root
        self.addCleanup(os.environ.pop, "TOFT_SCRATCH_DIR", None)

    def test_an_absolute_part_is_refused(self):
        self.assertRefused("bad_request", toft.scratch_dir, "/etc")
        self.assertRefused("bad_request", toft.scratch_dir, "a", "/etc")

    def test_dot_dot_is_refused_not_a_raw_permission_error(self):
        self.assertRefused("bad_request", toft.scratch_dir, "..", "x")
        self.assertRefused("bad_request", toft.scratch_dir, "a/../../x")
        self.assertFalse((self.root.parent / "x").exists())

    def test_a_symlink_out_is_refused(self):
        outside = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, outside, True)
        (self.root / "link").symlink_to(outside)
        self.assertRefused("bad_request", toft.scratch_dir, "link", "deeper")

    def test_a_dot_dot_that_stays_inside_is_fine(self):
        self.assertEqual(toft.scratch_dir("a", "..", "b").resolve(), (self.root / "b").resolve())
        self.assertEqual(toft.scratch_dir().resolve(), self.root.resolve())

    def test_a_non_path_part_is_refused(self):
        self.assertRefused("bad_request", toft.scratch_dir, 3)

    def test_session_scratch_parts_are_held_to_the_same_rule(self):
        self.assertRefused("bad_request", toft.session_scratch_dir, "sid", "..", "..", "x")


class TestOffPlatformScratchIsPrivate(unittest.TestCase):
    """BB-034: off-platform scratch is a 0700 directory of this user's own."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        saved = os.environ.pop("TOFT_SCRATCH_DIR", None)
        if saved is not None:
            self.addCleanup(os.environ.__setitem__, "TOFT_SCRATCH_DIR", saved)
        scratch_module._local = None
        self.addCleanup(setattr, scratch_module, "_local", None)
        patcher = mock.patch.object(tempfile, "gettempdir", return_value=self.tmp)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_not_the_shared_temp_directory(self):
        root = toft.scratch_dir()
        self.assertNotEqual(root.resolve(), Path(self.tmp).resolve())
        self.assertIn(Path(self.tmp).resolve(), root.resolve().parents)
        for path in (root, root.parent):
            mode = stat.S_IMODE(os.lstat(path).st_mode)
            self.assertEqual(mode & 0o077, 0, oct(mode))

    def test_a_planted_parent_is_not_adopted(self):
        # Another user (simulated: a world-writable directory) got there first.
        planted = Path(self.tmp) / f"toft-scratch-{os.getuid()}"
        planted.mkdir()
        os.chmod(planted, 0o777)
        root = toft.scratch_dir()
        self.assertNotIn(planted.resolve(), root.resolve().parents)
        self.assertEqual(stat.S_IMODE(os.lstat(root).st_mode) & 0o077, 0)

    def test_stable_within_the_process(self):
        self.assertEqual(toft.scratch_dir(), toft.scratch_dir())


class TestLocalFailuresAreCoded(Refusals, PlatformTestCase):
    """BB-007: a local failure carries a code and is not retryable."""

    def test_slack_file_of_a_missing_path(self):
        err = self.assertRefused("not_found", toft.notify.slack_file, "/nonexistent/report.xlsx")
        self.assertIsNone(err.status)
        self.assertEqual(FakePlatform.requests, [])

    def test_slack_file_of_a_directory(self):
        self.assertRefused("bad_request", toft.notify.slack_file, tempfile.gettempdir())

    def test_an_attachment_of_the_wrong_shape(self):
        self.assertRefused("bad_request", toft.notify.slack_file, 42)
        self.assertRefused("bad_request", toft.notify.slack_file, ("a.txt", 42))

    def test_send_through_email_is_coded(self):
        self.assertRefused("bad_request", toft.notify.send, "email", "hello")

    def test_store_upload_of_a_missing_file(self):
        os.environ["TOFT_STORAGE"] = '[{"name": "drop", "kind": "store", "access": "full"}]'
        self.assertRefused("not_found", toft.store("drop").upload, "/nonexistent/x.csv")
        self.assertEqual(FakePlatform.requests, [])

    def test_a_transport_failure_is_still_retryable(self):
        from toft import session
        from toft.transport import AppTransport

        session.reset(AppTransport(base_url="http://127.0.0.1:1", token="t"))
        with mock.patch("time.sleep"):
            with self.assertRaises(ToftError) as ctx:
                toft.data.get()
        self.assertTrue(ctx.exception.retryable)


class TestInputValidation(Refusals, PlatformTestCase):
    """Each of these is refused bad_request before any request is made."""

    def tearDown(self):
        self.assertEqual(FakePlatform.requests, [], "a request was made")
        super().tearDown()

    def test_email_to_none(self):
        self.assertRefused("bad_request", toft.notify.email, to=None, subject="s", text="t")
        self.assertRefused("bad_request", toft.notify.email, to=[None], subject="s", text="t")

    def test_data_set_not_serializable(self):
        self.assertRefused("bad_request", toft.data.set, {"x": object()})
        self.assertRefused("bad_request", toft.data.set, {"x": float("nan")})

    def test_jobs_start_params_not_a_mapping(self):
        self.assertRefused("param_type", toft.jobs.start, "reconcile", params=[1, 2])

    def test_jobs_start_params_not_serializable(self):
        self.assertRefused("bad_request", toft.jobs.start, "reconcile", params={"x": {1, 2}})

    def test_jobs_start_with_no_name(self):
        self.assertRefused("bad_request", toft.jobs.start, "")
        self.assertRefused("bad_request", toft.jobs.start, None)

    def test_jobs_get_with_no_id(self):
        self.assertRefused("bad_request", toft.jobs.get, "")

    def test_jobs_list_bogus_status(self):
        self.assertRefused("bad_request", toft.jobs.list, status="bogus")

    def test_jobs_list_limit_out_of_range(self):
        for limit in (0, -1, 201, True, "10", 1.5):
            self.assertRefused("bad_request", toft.jobs.list, limit=limit)

    def test_access_token_with_no_kind(self):
        self.assertRefused("bad_request", toft.access_token, "")
        self.assertRefused("bad_request", toft.access_token, None)

    def test_azure_credential_for_a_non_microsoft_kind(self):
        self.assertRefused("bad_request", toft.azure_credential, "gdrive")
        self.assertRefused("bad_request", toft.azure_credential, "slack")
        self.assertRefused("bad_request", toft.azure_credential, "")
        self.assertEqual(toft.azure_credential("sharepoint").kind, "sharepoint")
        self.assertEqual(toft.azure_credential("onedrive").kind, "onedrive")

    def test_app_can_with_no_name(self):
        self.assertRefused("bad_request", toft.app.can, "")
        self.assertRefused("bad_request", toft.app.can, None)


class TestValidListingsStillGoOut(PlatformTestCase):
    def test_a_known_status_and_limit_are_sent(self):
        self.route("GET /api/v1/app/job-runs", 200, {"runs": []})
        self.assertEqual(toft.jobs.list(status="queued", limit=200), [])
        self.assertEqual(toft.jobs.list(status="out_of_memory", limit=1), [])
        self.assertIn("status=queued", FakePlatform.requests[0]["path"])


class TestProgressRange(Refusals, unittest.TestCase):
    def test_progress_outside_0_to_100_is_refused(self):
        from toft.client import Run

        run = Run("http://127.0.0.1:1", "t", "run_1")
        for percent in (150, -1, 101, True, "50"):
            self.assertRefused("bad_request", run.progress, percent)

    def test_write_output_json_of_an_object(self):
        from toft.client import Run

        run = Run("http://127.0.0.1:1", "t", "run_1", output_uri="http://127.0.0.1:1/put")
        self.assertRefused("bad_request", run.write_output_json, object())
        self.assertRefused("bad_request", run.write_output, 12)


class TestRunOutput(PlatformTestCase):
    """BB-019: no output is None, an empty output is b"", a missing run raises."""

    def test_no_output_is_none(self):
        self.route("GET /api/v1/app/job-runs/run_1/output", 404,
                   {"error": "this run produced no output", "code": "no_output", "retryable": False})
        self.assertIsNone(JobRun.from_raw({"id": "run_1"}).output())

    def test_an_empty_output_is_empty_bytes(self):
        # A 200 straight from the route: the job wrote an empty output. The
        # fake writes JSON, so script a body of "" — which json-encodes to '""'.
        self.route("GET /api/v1/app/job-runs/run_1/output", 200, "")
        self.assertEqual(JobRun.from_raw({"id": "run_1"}).output(), b'""')

    def test_a_missing_run_raises_rather_than_reading_as_no_output(self):
        self.route("GET /api/v1/app/job-runs/nope/output", 404,
                   {"error": "no such run", "code": "not_found", "retryable": False})
        with self.assertRaises(ToftError) as ctx:
            JobRun.from_raw({"id": "nope"}).output()
        self.assertEqual(ctx.exception.code, "not_found")


class TestUpdateOutlastsTheLimiter(PlatformTestCase):
    """BB-008: many rate_limited refusals in a row do not lose the update."""

    def test_more_refusals_than_the_old_wait_count(self):
        limited = (429, {"code": "rate_limited", "retryable": True, "error": "slow down"},
                   {"Retry-After": "1"})
        self.script("GET /api/v1/app/data", [
            (200, {"data": {"runs": 1}, "version": 1, "bytes": 12}, None),
        ])
        self.script("PUT /api/v1/app/data", [limited] * 80 + [(200, {"version": 2, "bytes": 12}, None)])
        slept = []
        with mock.patch("time.sleep", side_effect=slept.append):
            out = toft.data.update(lambda s: s.update(runs=s.get("runs", 0) + 1), attempts=1)
        self.assertEqual(out.version, 2)
        # Retry-After was honoured: every pause at least the second it asked for.
        self.assertTrue(slept and min(slept) >= 1, slept[:5])

    def test_it_still_gives_up_at_its_deadline(self):
        data_module = sys.modules[type(toft.data).__module__]
        self.route("GET /api/v1/app/data", 429,
                   {"code": "rate_limited", "retryable": True, "error": "slow down"},
                   {"Retry-After": "1"})
        clock = [0.0]

        def advance(seconds):
            clock[0] += seconds

        fake = mock.Mock(sleep=advance, monotonic=lambda: clock[0])
        import toft.transport as transport_module

        with mock.patch.object(data_module, "time", fake), \
                mock.patch.object(transport_module, "time", fake):
            with self.assertRaises(ToftError) as ctx:
                toft.data.update(lambda s: s.update(a=1))
        self.assertEqual(ctx.exception.code, "rate_limited")
        self.assertGreaterEqual(clock[0], data_module.RATE_LIMIT_TIMEOUT)


if __name__ == "__main__":
    unittest.main()
