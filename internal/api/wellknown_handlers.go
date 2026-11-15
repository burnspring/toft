package api

import (
	"net/http"
	"strings"
	"time"

	"toft/internal/database"
)

// /.well-known/security.txt — RFC 9116.
//
// The third thing this platform owes an AppSec team, after "what is there to
// scan" (/admin/estate/targets) and "how do I not show up as usage"
// (X-Toft-Scan): where to send what they found. It is a file, served
// unauthenticated, and nothing here fetches anything.
//
// **On both origins.** A researcher who found something in a deployed app is
// looking at the apps origin and has no reason to know the platform has
// another one; a researcher who found something in Toft itself is on the
// platform origin. On a single-origin install the two are the same listener
// and this is mounted once. It is the second reserved path on the apps
// gateway, after the handshake, and it is declared before app routing for the
// same reason that one is — nothing an app serves may shadow it.
//
// **Absent, not empty.** With no contact configured the path is 404 rather
// than a file with blank fields. A contentless security.txt is a published
// promise that nobody is reading, which is worse than not publishing one.

const securityTxtPath = "/.well-known/security.txt"

// handleSecurityTxt serves the file, or 404s when nothing is configured.
func (s *Server) handleSecurityTxt(w http.ResponseWriter, r *http.Request) {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !settings.SecurityTxtConfigured() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// An hour, not robots.txt's day. The one thing that must not be stale here
	// is the contact, and an install that has just changed it after somebody
	// left should not be answering the old one for a day.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(renderSecurityTxt(settings)))
}

// renderSecurityTxt builds the file. Pure, so every field's shape is testable
// without a server.
//
// **The expiry is served as stored, even when it has passed.** The RFC's field
// is a record of what the install committed to, and a platform that quietly
// rolled it forward would be forging a commitment nobody made — the honest
// handling of a stale date is to publish it and say so loudly on the settings
// page, which is where somebody can do something about it.
func renderSecurityTxt(s database.PlatformSettings) string {
	var b strings.Builder
	b.WriteString("# Published by Toft from this install's settings.\n")
	b.WriteString("# https://www.rfc-editor.org/rfc/rfc9116\n\n")
	b.WriteString("Contact: " + strings.TrimSpace(s.SecurityTxtContact) + "\n")
	if v := securityTxtExpiresAt(s.SecurityTxtExpires); v != "" {
		b.WriteString("Expires: " + v + "\n")
	}
	if v := strings.TrimSpace(s.SecurityTxtPolicy); v != "" {
		b.WriteString("Policy: " + v + "\n")
	}
	if v := normalizeSecurityTxtLanguages(s.SecurityTxtLanguages); v != "" {
		b.WriteString("Preferred-Languages: " + v + "\n")
	}
	return b.String()
}

// securityTxtExpiresAt turns the stored date into the timestamp the RFC wants.
//
// The end of that day in UTC, because "expires on the 4th" means the 4th is
// still good — an administrator who typed a date and found the file dead at
// midnight of the morning before would have been given a control that means
// something other than what it says. Empty for a value that is not a date,
// which the field is then simply absent for: a malformed Expires is a file
// every validator rejects, and omitting it is the smaller lie.
func securityTxtExpiresAt(stored string) string {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(stored))
	if err != nil {
		return ""
	}
	return d.Add(24*time.Hour - time.Second).UTC().Format(time.RFC3339)
}

// normalizeSecurityTxtLanguages tidies the comma-separated list into the one
// shape the RFC's grammar accepts: tags separated by ", ".
func normalizeSecurityTxtLanguages(stored string) string {
	out := []string{}
	for _, part := range strings.Split(stored, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, ", ")
}
