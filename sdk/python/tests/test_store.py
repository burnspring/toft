"""The store accessor and Mount.put — sugar over the mount, and no second write path.

Standard library only, like the rest: ``python3 -m unittest discover -s tests``
from ``sdk/python``. The scenarios here are the ones the Node SDK must also
pass, so an app written in one language cannot observe a platform the other
cannot.
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import toft  # noqa: E402
from toft.env import ENV_STORAGE  # noqa: E402
from toft.errors import ToftError  # noqa: E402


class StoreTests(unittest.TestCase):
    def setUp(self) -> None:
        self.root = Path(tempfile.mkdtemp())
        self.mounted = self.root / "mounted"
        self.mounted.mkdir()
        self.source = self.root / "nightly.parquet"
        self.source.write_bytes(b"the nightly file")

        self.previous = os.environ.get(ENV_STORAGE)
        os.environ[ENV_STORAGE] = json.dumps(
            [
                {"name": "reports", "kind": "store", "path": str(self.mounted), "access": "write"},
                {"name": "cache", "kind": "volume", "path": str(self.mounted), "access": "full"},
                {"name": "archive", "kind": "store", "path": str(self.mounted), "access": "read"},
            ]
        )

    def tearDown(self) -> None:
        if self.previous is None:
            os.environ.pop(ENV_STORAGE, None)
        else:
            os.environ[ENV_STORAGE] = self.previous

    def test_put_writes_to_the_mounted_path(self) -> None:
        """It is sugar over the mount, so the file is simply there afterwards."""
        landed = toft.store("reports").put(self.source, "2026-08.parquet")
        self.assertEqual(landed, self.mounted / "2026-08.parquet")
        self.assertEqual(landed.read_bytes(), b"the nightly file")

    def test_put_defaults_to_the_files_own_name(self) -> None:
        landed = toft.store("reports").put(self.source)
        self.assertEqual(landed.name, "nightly.parquet")
        # An empty name is the same request as no name, rather than an error:
        # "" is not a name somebody meant, it is the absence of one.
        self.assertEqual(toft.store("reports").put(self.source, "").name, "nightly.parquet")

    def test_put_creates_intermediate_folders(self) -> None:
        landed = toft.store("reports").put(self.source, "2026/08/17.parquet")
        self.assertTrue(landed.exists())
        self.assertEqual(landed.parent, self.mounted / "2026" / "08")

    def test_a_name_cannot_escape_the_mount(self) -> None:
        """The name is the caller's and the location is not.

        A traversal segment here would write into the app's own filesystem —
        over its code, in the worst case — rather than into its storage.
        """
        for name in ["../escape", "a/../../escape", "/absolute", "a//b", "a/./b", "a/", ".."]:
            with self.subTest(name=name):
                with self.assertRaises(ToftError):
                    toft.store("reports").put(self.source, name)

    def test_a_read_only_mount_says_so(self) -> None:
        """A courtesy, not the enforcement — the mount is the enforcement."""
        with self.assertRaises(ToftError) as caught:
            toft.store("archive").put(self.source)
        self.assertIn("read-only", str(caught.exception))

    def test_store_refuses_a_volume_by_kind(self) -> None:
        with self.assertRaises(ToftError) as caught:
            toft.store("cache")
        self.assertIn("volume", str(caught.exception))

    def test_storage_still_answers_for_both_kinds(self) -> None:
        """App code that has a volume mounted has done nothing wrong."""
        self.assertEqual(toft.storage("cache").kind, "volume")
        self.assertEqual(toft.storage("reports").kind, "store")

    def test_a_volume_at_full_is_writable(self) -> None:
        """`full` is the only level above read a volume mapping has."""
        self.assertFalse(toft.storage("cache").read_only)

    def test_a_miss_names_what_is_mounted(self) -> None:
        with self.assertRaises(ToftError) as caught:
            toft.store("nope")
        self.assertIn("reports", str(caught.exception))


if __name__ == "__main__":
    unittest.main()


from test_app_sdk import FakePlatform, PlatformTestCase  # noqa: E402


class StoreOverURLTests(PlatformTestCase):
    """A store reached through the platform's signed URLs, in both modes.

    The signed URLs point back at the fake, so the object-store half of each
    call is recorded too — which is where the two things worth asserting live:
    the signed headers go verbatim, and the app's token does not go at all.
    """

    def setUp(self) -> None:
        super().setUp()
        self.previous = os.environ.get(ENV_STORAGE)
        os.environ[ENV_STORAGE] = json.dumps([
            # Shared mode: a store is not mounted, and says so with no path.
            {"name": "uploads", "kind": "store", "path": "", "access": "write", "folder": "in"},
            {"name": "cache", "kind": "volume", "path": "/data/cache", "access": "full"},
        ])
        self.source = Path(tempfile.mkdtemp()) / "report.csv"
        self.source.write_bytes(b"a,b\n1,2\n")

    def tearDown(self) -> None:
        if self.previous is None:
            os.environ.pop(ENV_STORAGE, None)
        else:
            os.environ[ENV_STORAGE] = self.previous
        super().tearDown()

    def signed(self, method: str, headers: dict | None = None) -> tuple:
        return (200, {"url": f"{self.base}/bucket/team/uploads/in/report.csv",
                      "method": method, "headers": headers or {}, "key": "report.csv"}, None)

    def test_an_unmounted_store_has_no_path_to_write_into(self) -> None:
        """Path("") is the current directory; handing it back would write into the app."""
        handle = toft.store("uploads")
        self.assertFalse(handle.mounted)
        with self.assertRaises(ToftError) as caught:
            _ = handle.path
        self.assertIn("upload()", str(caught.exception))
        with self.assertRaises(ToftError) as caught:
            handle.put(self.source)
        self.assertIn("upload()", str(caught.exception))

    def test_upload_signs_then_sends_the_bytes_without_the_token(self) -> None:
        FakePlatform.routes["POST /api/v1/app/store/uploads/objects"] = self.signed(
            "PUT", {"If-None-Match": "*"})
        FakePlatform.routes["PUT /bucket/team/uploads/in/report.csv"] = (200, {}, None)

        self.assertEqual(toft.store("uploads").upload(self.source), "report.csv")

        mint, put = FakePlatform.requests
        self.assertIn("folder=in", mint["path"])
        sent = json.loads(mint["body"])
        self.assertEqual(sent["size"], len(b"a,b\n1,2\n"))
        self.assertEqual(sent["sha256"], hashlib.sha256(b"a,b\n1,2\n").hexdigest())
        self.assertEqual(put["body"], b"a,b\n1,2\n")
        self.assertEqual(put["headers"].get("If-None-Match"), "*")
        self.assertNotIn("Authorization", put["headers"])

    def test_a_large_upload_is_streamed_with_its_length_declared(self) -> None:
        """Security review 2026-11-14, CS-11: never read whole, never chunked."""
        payload = os.urandom(3 * 1024 * 1024 + 17)
        self.source.write_bytes(payload)
        FakePlatform.routes["POST /api/v1/app/store/uploads/objects"] = self.signed("PUT")
        FakePlatform.routes["PUT /bucket/team/uploads/in/report.csv"] = (200, {}, None)

        def no_whole_read(*_a, **_k):
            raise AssertionError("the file was read whole")

        original = Path.read_bytes
        Path.read_bytes = no_whole_read  # type: ignore[method-assign]
        try:
            toft.store("uploads").upload(self.source)
        finally:
            Path.read_bytes = original  # type: ignore[method-assign]

        mint, put = FakePlatform.requests
        sent = json.loads(mint["body"])
        self.assertEqual(sent["size"], len(payload))
        self.assertEqual(sent["sha256"], hashlib.sha256(payload).hexdigest())
        self.assertEqual(sent["md5"], hashlib.md5(payload).hexdigest())
        self.assertEqual(put["body"], payload)
        headers = {k.lower(): v for k, v in put["headers"].items()}
        self.assertEqual(headers.get("content-length"), str(len(payload)))
        self.assertNotIn("transfer-encoding", headers)

    def test_write_onto_an_existing_name_says_so(self) -> None:
        # Started at `full` and lowered since: the store signs at today's level,
        # so the refusal names `write` — never the level the app started with.
        os.environ[ENV_STORAGE] = json.dumps([
            {"name": "uploads", "kind": "store", "path": "", "access": "full", "folder": "in"}])
        FakePlatform.routes["POST /api/v1/app/store/uploads/objects"] = self.signed(
            "PUT", {"If-None-Match": "*"})
        FakePlatform.routes["PUT /bucket/team/uploads/in/report.csv"] = (412, {}, None)
        with self.assertRaises(ToftError) as caught:
            toft.store("uploads").upload(self.source)
        self.assertIn("already exists", str(caught.exception))
        self.assertIn("`write`", str(caught.exception))
        self.assertNotIn("full", str(caught.exception))
        self.assertIn("new name", caught.exception.hint)

    def test_url_and_download_are_signed_gets(self) -> None:
        FakePlatform.routes["GET /api/v1/app/store/uploads/objects/2026/report.csv"] = self.signed("GET")
        FakePlatform.routes["GET /bucket/team/uploads/in/report.csv"] = (200, "the bytes", None)

        link = toft.store("uploads").url("2026/report.csv", attachment=True)
        self.assertTrue(link.endswith("/bucket/team/uploads/in/report.csv"))
        self.assertIn("disposition=attachment", FakePlatform.requests[-1]["path"])

        self.assertEqual(toft.store("uploads").download("2026/report.csv"), b'"the bytes"')

    def test_a_download_of_a_missing_object_is_not_found(self) -> None:
        FakePlatform.routes["GET /api/v1/app/store/uploads/objects/sales.db"] = self.signed("GET")
        FakePlatform.routes["GET /bucket/team/uploads/in/report.csv"] = (404, {}, None)
        with self.assertRaises(ToftError) as caught:
            toft.store("uploads").download("sales.db")
        self.assertEqual(caught.exception.code, "not_found")
        self.assertFalse(caught.exception.retryable)
        self.assertIn("'sales.db'", str(caught.exception))
        self.assertIn("list()", caught.exception.hint)

    def test_link_asks_the_platform_and_carries_the_folder(self) -> None:
        FakePlatform.routes["GET /api/v1/app/store/uploads/link/2026/q1%20report.csv"] = (
            200, {"link": "https://toft.example/files/uploads/in/2026/q1%20report.csv", "name": "2026/q1 report.csv"}, None)
        link = toft.store("uploads").link("2026/q1 report.csv")
        self.assertEqual(link, "https://toft.example/files/uploads/in/2026/q1%20report.csv")
        self.assertIn("folder=in", FakePlatform.requests[-1]["path"])

    def test_a_link_name_cannot_leave_the_store(self) -> None:
        for name in ["../escape", "/abs", "a//b"]:
            with self.subTest(name=name), self.assertRaises(ToftError):
                toft.store("uploads").link(name)
        self.assertEqual(FakePlatform.requests, [])

    def test_a_name_cannot_leave_the_store(self) -> None:
        for name in ["../escape", "/abs", "a//b"]:
            with self.subTest(name=name), self.assertRaises(ToftError):
                toft.store("uploads").url(name)
        self.assertEqual(FakePlatform.requests, [])

    def test_a_volume_is_not_reached_over_urls(self) -> None:
        with self.assertRaises(ToftError):
            toft.storage("cache").list()
        self.assertEqual(FakePlatform.requests, [])


class SDKRefusalEnvelopeTests(unittest.TestCase):
    """The SDK's own refusals carry a code and retryable False — a wrong name or
    a missing mount is not a blip, and a statusless error would otherwise read
    as one."""

    def setUp(self) -> None:
        self.previous = os.environ.get(ENV_STORAGE)
        os.environ[ENV_STORAGE] = json.dumps([
            {"name": "drop", "kind": "store", "path": "", "access": "read"},
            {"name": "drop", "kind": "store", "path": "", "access": "write", "folder": "backup"},
        ])

    def tearDown(self) -> None:
        if self.previous is None:
            os.environ.pop(ENV_STORAGE, None)
        else:
            os.environ[ENV_STORAGE] = self.previous

    def test_each_refusal_is_coded(self) -> None:
        cases = [
            (lambda: toft.store("drop", folder="").path, "not_mounted"),
            (lambda: toft.store("drop"), "ambiguous_mount"),
            (lambda: toft.storage("nope"), "not_mapped"),
            (lambda: toft.store("drop", folder="elsewhere"), "not_mapped"),
            (lambda: toft.store(""), "invalid_name"),
            (lambda: toft.store("drop", folder="").put(__file__, "../x"), "invalid_name"),
        ]
        for fn, code in cases:
            with self.subTest(code=code):
                with self.assertRaises(toft.ToftError) as ctx:
                    fn()
                self.assertEqual(ctx.exception.code, code)
                self.assertFalse(ctx.exception.retryable)
                # Each one the SDK raises has a next step, so it names one
                # (drills 2026-10-28 #10: the hint was null on every one).
                self.assertTrue(ctx.exception.hint, f"{code} has no hint")

    def test_ambiguous_mount_hint_names_the_folder_argument(self) -> None:
        with self.assertRaises(toft.ToftError) as ctx:
            toft.store("drop")
        self.assertIn("toft.store('drop', folder=", ctx.exception.hint)

    def test_ambiguous_mount_names_the_accessor_the_caller_used(self) -> None:
        # store("x") was told to call toft.storage(...), which is not what a
        # store wants (drills D17).
        for fn, want in ((toft.store, "toft.store('drop', folder="),
                         (toft.storage, "toft.storage('drop', folder=")):
            with self.subTest(accessor=fn.__name__):
                with self.assertRaises(toft.ToftError) as ctx:
                    fn("drop")
                self.assertIn(want, str(ctx.exception))
