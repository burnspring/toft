"""The role header, from the app's side of it."""

from __future__ import annotations

import base64
import hashlib
import hmac
import time

import pytest

from toft.caller import (
    ENV_ROLE_KEY,
    ROLE_ANONYMOUS,
    ROLE_APP,
    ROLE_EDITOR,
    ROLE_HEADER,
    ROLE_OWNER,
    ROLE_SYSTEM,
    ROLE_UNKNOWN,
    ROLE_VIEWER,
    Caller,
    caller,
    verify_role,
)

KEY = "test-role-key"
LADDER = [ROLE_ANONYMOUS, ROLE_VIEWER, ROLE_EDITOR, ROLE_OWNER]


def mint(key: str, role: str, expiry: int) -> str:
    """The platform's mint, spelled here so this suite needs nothing from Go.

    ``sdk/rolevector_test.go`` is what proves the two spellings agree against a
    committed vector; this only needs *a* valid blob.
    """
    app_id = base64.urlsafe_b64encode(b"app-a").decode().rstrip("=")
    payload = f"v1.{role}.{app_id}.{expiry}"
    mac = hmac.new(key.encode(), b"toft-role\x00" + payload.encode(), hashlib.sha256).digest()
    return payload + "." + base64.urlsafe_b64encode(mac).decode().rstrip("=")


def soon() -> int:
    return int(time.time()) + 60


# The ladder is people only, and everything it cannot place is False.
#
# The temptation this refuses is ranking ``system`` and ``app`` somewhere: they
# are not more or less authorized than a person, they are a different kind of
# caller, and putting Toft's scheduler above an owner would be a guess the
# SDK has no business making on an app's behalf.
def test_at_least_answers_only_about_people():
    for i, role in enumerate(LADDER):
        for j, want in enumerate(LADDER):
            assert Caller(role).at_least(want) is (i >= j), f"{role}.at_least({want})"

    for role in (ROLE_SYSTEM, ROLE_APP, ROLE_UNKNOWN):
        for want in LADDER:
            assert Caller(role).at_least(want) is False, f"{role}.at_least({want})"

    # Asking "at least system?" is the wrong question and must not be answered
    # yes however authorized the caller is.
    assert Caller(ROLE_OWNER).at_least(ROLE_SYSTEM) is False


# A missing header refuses, and that is the whole safety property: the one
# deployment where it is missing is the one nobody configured.
def test_an_unverifiable_caller_is_unknown_and_unknown_refuses(monkeypatch):
    monkeypatch.delenv(ENV_ROLE_KEY, raising=False)
    off = caller({})
    assert off.role == ROLE_UNKNOWN
    assert off.at_least(ROLE_ANONYMOUS) is False
    assert off.is_person is False
    assert off.signed_in is False
    assert off.is_system is False

    monkeypatch.setenv(ENV_ROLE_KEY, KEY)
    # A bare word rather than a signed assertion.
    assert caller({ROLE_HEADER: "owner"}).role == ROLE_UNKNOWN
    # The real thing, read out of the canonical spelling and out of the
    # lowercased one a framework may hand over.
    assert caller({ROLE_HEADER: mint(KEY, ROLE_EDITOR, soon())}).role == ROLE_EDITOR
    assert caller({"x-toft-role": mint(KEY, ROLE_OWNER, soon())}).role == ROLE_OWNER


# Bound to one key and one moment — the two properties the whole design rests
# on, from the app's side of them.
@pytest.mark.parametrize(
    "key,blob_role,expiry,want",
    [
        (KEY, ROLE_OWNER, "soon", ROLE_OWNER),
        ("another-app-key", ROLE_OWNER, "soon", ROLE_UNKNOWN),
        (KEY, ROLE_OWNER, "past", ROLE_UNKNOWN),
        ("", ROLE_OWNER, "soon", ROLE_UNKNOWN),
    ],
)
def test_a_wrong_key_and_a_stale_blob_are_both_unknown(key, blob_role, expiry, want):
    blob = mint(KEY, blob_role, soon() if expiry == "soon" else 1)
    assert verify_role(key, blob, time.time()) == want


def test_an_absent_blob_is_unknown():
    assert verify_role(KEY, "", time.time()) == ROLE_UNKNOWN


# A blob signed with this app's own key still cannot introduce a word the SDK
# does not know. The app holds the key, so this is the one forgery it can
# actually mount — and while an app lying to its own handler crosses no
# privilege, app code branching on six values must not be handed a seventh.
def test_a_signed_word_the_sdk_does_not_know_is_unknown():
    assert verify_role(KEY, mint(KEY, "superuser", soon()), time.time()) == ROLE_UNKNOWN
    # The same mint with a known word works, so the assertion above is about the
    # word rather than about a broken signature.
    assert verify_role(KEY, mint(KEY, ROLE_EDITOR, soon()), time.time()) == ROLE_EDITOR


# The one bit a public app gains: a signed-in reader is tellable from a
# stranger. It names nobody, and it is exactly what "refuse anonymous visitors"
# is.
def test_signed_in_separates_a_stranger_from_a_reader():
    assert Caller(ROLE_ANONYMOUS).signed_in is False
    assert Caller(ROLE_ANONYMOUS).is_person is True
    assert Caller(ROLE_VIEWER).signed_in is True
    assert Caller(ROLE_SYSTEM).signed_in is False
    assert Caller(ROLE_UNKNOWN).signed_in is False


# The scheduled-task line from the doc comment, run: an endpoint serving both a
# person and its own schedule needs both halves, and neither admits the other.
@pytest.mark.parametrize(
    "role,want",
    [
        (ROLE_SYSTEM, True),
        (ROLE_OWNER, True),
        (ROLE_EDITOR, True),
        (ROLE_VIEWER, False),
        (ROLE_ANONYMOUS, False),
        (ROLE_APP, False),
        (ROLE_UNKNOWN, False),
    ],
)
def test_a_scheduled_firing_is_not_a_person(role, want):
    c = Caller(role)
    assert (c.role == ROLE_SYSTEM or c.at_least(ROLE_EDITOR)) is want


def test_a_non_ascii_signature_is_unknown_not_a_type_error():
    """The header is anything a neighbour on the box chose to send.

    WSGI hands it over as latin-1 text, and ``hmac.compare_digest`` raises
    TypeError on a non-ASCII str; the contract is "never raises".
    """
    blob = mint(KEY, ROLE_OWNER, int(time.time()) + 60)
    forged = blob.rpartition(".")[0] + ".\xe9"
    assert verify_role(KEY, forged, time.time()) == ROLE_UNKNOWN
