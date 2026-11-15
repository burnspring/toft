"""The shared TOFT_STORAGE and respond vectors (sdk/testdata), Python's half.

Every SDK reads the same files, so a language that drifts — dropping an entry
that is not an object, taking a kind the platform never writes, answering 202
for a run start() did not return — fails its own suite rather than a
black-box run.
"""

from __future__ import annotations

import json
import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import toft  # noqa: E402
from toft.errors import ToftError  # noqa: E402
from toft.jobs import JobRun  # noqa: E402
from toft.resources import mounts  # noqa: E402

TESTDATA = Path(__file__).resolve().parent.parent.parent / "testdata"


def _read(name: str) -> dict:
    return json.loads((TESTDATA / name).read_text())


def _entry(m) -> dict:
    return {"name": m.name, "kind": m.kind, "path": m.mount_path, "access": m.access,
            "folder": m.folder, "read_only": m.read_only, "mounted": m.mounted}


class TestTheSharedStorageEnvVector(unittest.TestCase):
    def test_every_case(self):
        cases = _read("storage-env-vector.json")["cases"]
        self.assertTrue(cases)
        for tc in cases:
            with self.subTest(tc["name"]):
                env = {k: v for k, v in os.environ.items() if k != "TOFT_STORAGE"}
                if tc["raw"] is not None:
                    env["TOFT_STORAGE"] = tc["raw"]
                with mock.patch.dict(os.environ, env, clear=True):
                    if "error" in tc:
                        with self.assertRaises(ToftError) as caught:
                            mounts()
                        self.assertEqual(caught.exception.code, tc["error"], str(caught.exception))
                        self.assertTrue(caught.exception.hint, f"{caught.exception} has no hint")
                    else:
                        self.assertEqual([_entry(m) for m in mounts()], tc["mounts"])


class TestTheSharedRespondVector(unittest.TestCase):
    def test_every_case(self):
        for tc in _read("respond-vector.json")["cases"]:
            with self.subTest(tc["name"]):
                run = tc["string"] if "string" in tc else JobRun.from_raw(tc["run"])
                for accept, want in (("application/json", tc.get("json")), ("text/html", tc.get("html"))):
                    request = {"Accept": accept}
                    if "error" in tc:
                        with self.assertRaises(ToftError) as caught:
                            toft.jobs.respond(run, request)
                        self.assertEqual(caught.exception.code, tc["error"], str(caught.exception))
                        self.assertTrue(caught.exception.hint, f"{caught.exception} has no hint")
                        continue
                    _body, status, headers = toft.jobs.respond(run, request)
                    self.assertEqual(status, want["status"], accept)
                    self.assertEqual(headers.get("Location"), want["location"], accept)


if __name__ == "__main__":
    unittest.main()
