"""Regressions for the 2026-11-04 black-box list's Python findings
(BB-091, BB-092, BB-096)."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import toft  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.resources import Mount  # noqa: E402

from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402


def _refused(test, call, code):
    FakePlatform.requests = []
    with test.assertRaises(ToftError) as caught:
        call()
    test.assertEqual(caught.exception.code, code, str(caught.exception))
    test.assertTrue(caught.exception.hint, f"{code} ({caught.exception}) has no hint")
    test.assertEqual(FakePlatform.requests, [], "a refusal was sent")
    return caught.exception


class TestParamsNotAnObjectIsParamType(PlatformTestCase):
    """BB-091: "x", [1] or 5 is param_type, as the platform says."""

    def test_param_type(self):
        for wrong in ("x", [1], 5):
            _refused(self, lambda: toft.jobs.start("np", params=wrong), "param_type")


class TestNotifyRefusesWhatItDoesNotTake(PlatformTestCase):
    """BB-092: a destination or sender field is coded, never a TypeError."""

    def test_destination(self):
        err = _refused(self, lambda: toft.notify.send("teams", "hi", channel="#x"),
                       "destination_not_allowed")
        self.assertIn("Microsoft Teams", str(err))
        _refused(self, lambda: toft.notify.slack("hi", channel="#x"), "destination_not_allowed")

    def test_sender(self):
        _refused(self, lambda: toft.notify.email(to="a@example.com", subject="s", text="t",
                                                 from_="x@example.com"), "sender_not_allowed")
        _refused(self, lambda: toft.notify.email(to="a@example.com", subject="s", text="t",
                                                 reply_to="x@example.com"), "sender_not_allowed")

    def test_email_through_send(self):
        _refused(self, lambda: toft.notify.send("email", to="a@example.com"), "bad_request")

    def test_anything_else(self):
        err = _refused(self, lambda: toft.notify.send("slack", "hi", colour="red"), "bad_request")
        self.assertIn("colour", str(err))


class TestMountReprMatchesToDict(unittest.TestCase):
    """BB-096: an unmounted store's path reads "" in repr as in to_dict()."""

    def test_unmounted(self):
        m = Mount(name="s", kind="store", access="read")
        self.assertIn("path=''", repr(m))
        self.assertEqual(m.to_dict()["path"], "")


if __name__ == "__main__":
    unittest.main()
