package api

import (
	"net/http"
	"strings"
)

// The reserved slug namespace.
//
// A slug beginning with `_` is refused at every path that creates or renames an
// app, for every caller including administrators — and nothing lives there.
// Toft's own manuals did, as apps installed from the gallery onto
// `/apps/_user-guide` and its two siblings; they are a public site the product
// links to now (internal/docs), and the gallery has no door into `_` any more.
//
// # Why it stays reserved with nothing in it
//
// Because the two directions are not symmetric. Keeping it costs one validator
// branch and a test. Releasing it would let somebody create `_user-guide` on an
// install where that address is still in bookmarks and in the old guide's own
// pages, and taking it back afterwards costs migrating their app's URL. A
// namespace held empty is cheaper than one handed back and later wanted.
//
// An app an earlier version installed there is an ordinary app now: listed,
// readable on the same gates as any other, and deletable by its owner.
//
// # Why `_`
//
// It is correct rather than chosen. `slugRe` is `[^a-z0-9-]` and sanitizeSlug
// *replaces* rather than rejects, then trims leading and trailing `-`, so a user
// typing `_user-guide` already gets `user-guide`. The character is unreachable
// from every existing path, which made the reservation a validator branch and a
// test rather than a migration or a route change.
//
// # Why it is enforced explicitly anyway
//
// Because relying on that normalization would make the namespace open the moment
// somebody loosens `slugRe` — a one-character change with no visible connection
// to this file. Fail closed, and let a test be the mechanism:
// TestReservedSlugNamespace walks every creation path.
const reservedSlugPrefix = "_"

// isReservedSlug reports whether a slug belongs to the platform's namespace.
//
// Pure, and takes the slug as it will actually be stored — after sanitizing,
// after uniquifying — because that is the value the reservation is about. A
// caller that checked the raw input would still let `ensureUniqueSlug` hand back
// `_user-guide-2`.
func isReservedSlug(slug string) bool {
	return strings.HasPrefix(slug, reservedSlugPrefix)
}

// reservedSlugMessage is the refusal, in one place so every path words it the
// same way. It names what would work, which is the standing rule for a refusal
// somebody can act on.
const reservedSlugMessage = "a slug starting with \"_\" is reserved by Toft " +
	"— choose a name that starts with a letter or a digit"

// rejectReservedSlug answers the request and reports true when the slug is
// reserved. 400 rather than 403: the request is malformed, not forbidden — no
// caller on this install, administrator included, may have this name.
func rejectReservedSlug(w http.ResponseWriter, slug string) bool {
	if !isReservedSlug(slug) {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": reservedSlugMessage,
		"code":  "reserved_slug", "hint": "choose a slug that starts with a letter or a digit",
	})
	return true
}
