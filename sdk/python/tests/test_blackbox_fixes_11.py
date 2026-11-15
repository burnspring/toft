"""Regressions for the 2026-11-06/07 black-box runs, Python's side
(BB-85, BB-89)."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from toft import resources  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.transport import encode_json  # noqa: E402


class TestEveryStoreCallHasANameExample(unittest.TestCase):
    """BB-85: link() had no entry, so link(None) was KeyError, not bad_request."""

    def test_each_checked_call_is_in_the_table(self):
        src = Path(resources.__file__).read_text()
        import re
        calls = set(re.findall(r'_check_name_type\([^,]+,\s*"([a-z_]+)"', src))
        self.assertIn("link", calls)
        self.assertEqual(calls - set(resources._NAME_EXAMPLE), set())

    def test_an_unlisted_call_still_refuses_coded(self):
        with self.assertRaises(ToftError) as caught:
            resources._check_name_type(None, "somecall")
        self.assertEqual(caught.exception.code, "bad_request")
        self.assertIn('somecall("', caught.exception.hint)


class TestBodiesAreCompact(unittest.TestCase):
    """BB-89: the app-data cap counts bytes sent; Python's spaced, escaped
    JSON hit it ~18% before Node, Go and R."""

    def test_compact_utf8(self):
        self.assertEqual(encode_json({"a": 1, "b": "é"}), b'{"a":1,"b":"\xc3\xa9"}')


if __name__ == "__main__":
    unittest.main()
