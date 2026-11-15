"""A refusal the SDK makes before any request is coded and never retryable.

Without a body the status rule reads a statusless error as a network failure —
code "" and retryable True — which is the answer an agent's loop retries
forever.
"""

from __future__ import annotations

import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import toft  # noqa: E402
from toft.errors import ToftError  # noqa: E402


class LocalRefusals(unittest.TestCase):
    def assertRefused(self, code, fn, *args, **kwargs):
        with self.assertRaises(ToftError) as ctx:
            fn(*args, **kwargs)
        self.assertEqual(ctx.exception.code, code, str(ctx.exception))
        self.assertFalse(ctx.exception.retryable)

    def test_viewer_with_no_header(self):
        self.assertRefused("no_viewer", toft.viewer, {})

    def test_email_with_no_recipient(self):
        self.assertRefused("no_recipient", toft.notify.email, to=[], subject="subject", text="text")

    def test_status_with_no_id(self):
        self.assertRefused("bad_request", toft.notify.status, "")

    def test_data_set_a_list(self):
        self.assertRefused("bad_request", toft.data.set, [1, 2])

    def test_session_scratch_dir_with_no_id(self):
        self.assertRefused("bad_request", toft.session_scratch_dir, "")

    def test_no_viewer_says_how_to_act_as_the_app(self):
        with self.assertRaises(ToftError) as ctx:
            toft.viewer({})
        self.assertEqual(ctx.exception.hint, "use toft.jobs.start(...) to start it as the app, or toft.jobs.list(...) for its runs")

    def test_no_viewer_does_not_say_only_signed_in_sessions_carry_the_header(self):
        # An anonymous visitor to a public app carries one too, and its start
        # is the platform's viewer_anonymous (drills D5).
        with self.assertRaises(ToftError) as ctx:
            toft.viewer({})
        self.assertIn("anonymous visitor", str(ctx.exception))
        self.assertIn("viewer_anonymous", str(ctx.exception))
        self.assertNotIn("only for a signed-in", str(ctx.exception))

    def test_wait_inside_a_request_it_would_outlive(self):
        from toft.jobs import JobRun

        run = JobRun.from_raw({"id": "r"})
        self.assertRefused("would_outlive_request", run.wait,
                           headers={"X-Toft-Request-Deadline": "2000-01-01T00:00:00Z"})

    def test_respond_given_a_job_name_is_refused_coded(self):
        # BB-067: was an AttributeError from inside the SDK, and a 500.
        self.assertRefused("bad_request", toft.jobs.respond, "probe")

    def test_storage_env_hint_names_real_access_levels(self):
        # BB-071: "readwrite" is no level; read, write and full are.
        import os
        from unittest import mock

        with mock.patch.dict(os.environ, {"TOFT_STORAGE": '[{"name":"x"}]'}):
            with self.assertRaises(ToftError) as ctx:
                toft.storage("x")
        self.assertEqual(ctx.exception.code, "storage_env_invalid")
        self.assertNotIn("readwrite", ctx.exception.hint)
        self.assertIn("full", ctx.exception.hint)


if __name__ == "__main__":
    unittest.main()
