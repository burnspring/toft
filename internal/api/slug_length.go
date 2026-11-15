package api

import (
	"fmt"
	"net/http"
	"strings"
)

// maxSlugLen is the longest slug an app may have. An isolated release is a
// Kubernetes workload named builder.ReleaseProcessName — the slug, a dash and
// eight hex characters of the deployment — and an object name is a DNS label
// of at most 63. A longer slug was accepted at create and then had its release
// suffix cut off by kube.objectName, so every release of the app had the same
// object name. One limit for every install, so an app can move between them.
const maxSlugLen = 63 - len("-") - 8

// slugTooLongMessage is the refusal's sentence, for the create routes, a
// rename and the New App form's availability check alike.
func slugTooLongMessage(slug string) string {
	return fmt.Sprintf("a slug can be at most %d characters, and %q has %d — choose a shorter one",
		maxSlugLen, slug, len(slug))
}

// rejectLongSlug refuses a slug the caller stated that does not fit. A slug the
// platform derived from a name is cut to fit instead (fitSlug): nobody chose
// it, so there is nothing to refuse.
func rejectLongSlug(w http.ResponseWriter, slug string) bool {
	if len(slug) <= maxSlugLen {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": slugTooLongMessage(slug),
		"code":  "slug_too_long", "hint": "choose a shorter slug, or leave slug out and one is made from the name",
	})
	return true
}

// fitProposedSlug is a supplied slug's length rule, which turns on whether the
// caller *stated* it. `slug_exact` means a person named it (`--app`, toft.toml),
// so a long one is refused. Without it the slug is a proposal — `toft create`
// sends a folder's or a directory's name — and a proposal is cut to fit, as a
// name the platform derives itself is: refusing a 71-character folder name told
// somebody to rename a directory they never offered as a slug (drills,
// 2026-10-23). A stated slug that is not a slug at all is refused too
// (invalid_slug) rather than rewritten into one nobody named (third
// 2026-11-03 black-box list, BB-006). It reports false when it has written
// the refusal.
func fitProposedSlug(w http.ResponseWriter, raw string, exact bool) (string, bool) {
	slug := sanitizeSlug(raw)
	if exact {
		if slug != raw {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("%q is not a slug — a slug is lowercase letters, digits and dashes", raw),
				"code":  "invalid_slug",
				"hint":  fmt.Sprintf("name the app %q, or another slug of lowercase letters, digits and dashes", slug),
			})
			return "", false
		}
		return slug, !rejectLongSlug(w, slug)
	}
	return fitSlug(slug, maxSlugLen), true
}

// fitSlug cuts slug to n characters without leaving a trailing dash.
func fitSlug(slug string, n int) string {
	if len(slug) <= n {
		return slug
	}
	return strings.TrimRight(slug[:n], "-")
}
