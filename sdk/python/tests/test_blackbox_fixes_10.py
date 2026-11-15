"""Regressions for the 2026-11-04 black-box list, Python's side
(BB-005 notify, BB-008, BB-016)."""

from __future__ import annotations

import json
import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft.env import ENV_STORAGE  # noqa: E402
from toft.errors import ToftError  # noqa: E402

from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402


def _refused(test, call, code="bad_request"):
    FakePlatform.requests = []
    with test.assertRaises(ToftError) as caught:
        call()
    err = caught.exception
    test.assertEqual(err.code, code, str(err))
    test.assertFalse(err.retryable, str(err))
    test.assertTrue(err.hint, f"{code} ({err}) has no hint")
    test.assertEqual(FakePlatform.requests, [], "a refusal was sent")
    return err


class TestNotifyMissingArgumentsAreCoded(PlatformTestCase):
    """BB-005: a notify call missing a required argument is bad_request naming
    it, never Python's uncoded TypeError."""

    def test_email_without_text(self):
        err = _refused(self, lambda: toft.notify.email(to="a@example.com", subject="s", html="<b>x</b>"))
        self.assertIn("text", str(err))

    def test_email_without_subject(self):
        err = _refused(self, lambda: toft.notify.email(to="a@example.com", text="t"))
        self.assertIn("subject", str(err))

    def test_email_without_to(self):
        err = _refused(self, lambda: toft.notify.email(subject="s", text="t"))
        self.assertIn("to", str(err))

    def test_slack_without_text(self):
        _refused(self, lambda: toft.notify.slack())

    def test_the_others_without_their_arguments(self):
        for call in (lambda: toft.notify.send(), lambda: toft.notify.resolve(),
                     lambda: toft.notify.resolve("pagerduty"), lambda: toft.notify.deliver(),
                     lambda: toft.notify.deliver("sharepoint"), lambda: toft.notify.slack_file(),
                     lambda: toft.notify.status()):
            _refused(self, call)

    def test_a_complete_email_still_sends(self):
        self.route("POST /api/v1/app/notify/email", 202, {"id": "s1", "status": "queued"})
        self.assertEqual(toft.notify.email(to="a@example.com", subject="s", text="t").id, "s1")


class TestAnEmptyJobNameIsRefused(PlatformTestCase):
    """BB-016: jobs.list(job="") answered every run — the filter silently dropped."""

    def test_list(self):
        _refused(self, lambda: toft.jobs.list(job=""))
        _refused(self, lambda: toft.jobs.list(job="  "))

    def test_mine(self):
        _refused(self, lambda: toft.viewer({"X-Toft-Act-For": "blob"}).jobs.mine(job=""))

    def test_no_job_still_lists_every_run(self):
        self.route("GET /api/v1/app/job-runs", 200, {"runs": [{"id": "r1"}]})
        self.assertEqual([r.id for r in toft.jobs.list()], ["r1"])


class TestNotAStoreNamesTheFolder(unittest.TestCase):
    """BB-008: a volume mapped twice, store(name, folder="") was told to use
    toft.storage(name).path — which raises ambiguous_mount."""

    def _env(self, entries):
        return mock.patch.dict(os.environ, {ENV_STORAGE: json.dumps(entries)})

    def test_mapped_twice(self):
        entries = [
            {"name": "vol", "kind": "volume", "path": "/mnt/a", "access": "read"},
            {"name": "vol", "kind": "volume", "path": "/mnt/b", "access": "write", "folder": "x"},
        ]
        with self._env(entries):
            for folder in ("", "x"):
                with self.subTest(folder=folder):
                    with self.assertRaises(ToftError) as ctx:
                        toft.store("vol", folder=folder)
                    self.assertEqual(ctx.exception.code, "not_a_store")
                    want = f"toft.storage('vol', folder={folder!r})"
                    self.assertIn(want, ctx.exception.hint)
                    # The call the hint names resolves.
                    self.assertEqual(toft.storage("vol", folder=folder).folder, folder)
            # The signed-URL methods of the volume's Mount say the same (their
            # route check; off the platform url() is off_platform first).
            with self.assertRaises(ToftError) as ctx:
                toft.storage("vol", folder="")._route("/api/v1/app/store/{slug}/objects/{name}", "f")
            self.assertEqual(ctx.exception.code, "not_a_store")
            self.assertIn("toft.storage('vol', folder='')", ctx.exception.hint)

    def test_mapped_once_keeps_the_bare_name(self):
        with self._env([{"name": "vol", "kind": "volume", "path": "/mnt/a", "access": "read"}]):
            with self.assertRaises(ToftError) as ctx:
                toft.store("vol")
            self.assertIn("toft.storage('vol').path", ctx.exception.hint)


if __name__ == "__main__":
    unittest.main()
