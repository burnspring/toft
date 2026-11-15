package api

import (
	"log"
	"log/slog"
	"strings"

	"toft/internal/database"
	"toft/internal/mail"
	"toft/internal/notify"
	"toft/internal/sources"
)

// Telling somebody that where this install may reach for code has widened.
//
// **This is a notification and it is not a control.** An administrator — or
// somebody holding their session — can widen the source allowlist, and after
// connections the credential for whatever they allowed is already in a row.
// A second pair of eyes would be the control; it needs an approval model this
// platform does not have, for one field, while every other administrator action
// on this install stays single-handed. So what is built shortens how long such
// a change goes unnoticed, prevents nothing, and this file does not pretend
// otherwise.
//
// The real controls are unchanged and are what they are: creation and removal
// are audited with the diff, the clone credential appears in no response and no
// log, `BaseEnv`'s allow-list keeps it out of every workload, and a GitHub
// organization can revoke the installation from its own settings without asking
// this install for anything.
//
// Per the notification invariant the send is **optional and never
// load-bearing**: a channel that is absent, or that fails, changes nothing about
// whether the connection was created or the policy written. Every path here
// logs and returns.
func (s *Server) notifySourceReachWidened(title, detail string) {
	link := strings.TrimRight(s.cfg.BaseURL, "/") + "/admin/sources"
	data := notify.TemplateData{
		BaseURL:     s.cfg.BaseURL,
		EventTitle:  title,
		EventDetail: detail,
		Link:        link,
	}

	if s.mailer != nil && s.mailer.Enabled() {
		data.BaseURL = s.mailer.BaseURL()
		data.Link = strings.TrimRight(s.mailer.BaseURL(), "/") + "/admin/sources"
		// Addressed here, as every other administrator notice is: the mailer
		// fills recipients itself only when an admin group address is set, so
		// without one a message with no To reached nobody and logged nothing.
		to, err := s.mailer.AdminRecipients()
		switch {
		case err != nil:
			log.Printf("sources: listing admin recipients for the widening notice: %v", err)
		case len(to) == 0:
			slog.Warn("source reach widened, but no administrator has an email address on file and no admin group address is set")
		default:
			if msg, err := mail.Render("source_reach_widened", data); err != nil {
				log.Printf("sources: rendering the widening notice: %v", err)
			} else {
				msg.To = to
				s.mailer.SendAsync(msg)
			}
		}
	}
	// An install-level message, so no app id: an app's channel is for that
	// app, and this is about the install.
	s.postPlatform("source_reach_widened", "", func(base string) notify.TemplateData {
		d := data
		d.BaseURL = base
		d.Link = strings.TrimRight(base, "/") + "/admin/sources"
		return d
	})
}

// widenedPolicy reports how the source policy widened, or "" when it did not.
//
// **Only a widening is announced.** Narrowing is a control being tightened and
// needs nobody's attention at 3am; announcing both would train an administrator
// to ignore the message, which is the one failure a notification like this has.
func widenedPolicy(before, after database.SourcePolicy, beforePatterns, afterPatterns []string) string {
	var notes []string

	// `any` is the widest; `none` the narrowest. Only a move outward counts.
	rank := map[string]int{sources.ModeNone: 0, sources.ModeAllowlist: 1, sources.ModeAny: 2}
	if rank[after.Mode] > rank[before.Mode] {
		notes = append(notes, "The source policy moved from `"+before.Mode+"` to `"+after.Mode+"`.")
	}

	had := map[string]bool{}
	for _, p := range beforePatterns {
		had[p] = true
	}
	var added []string
	for _, p := range afterPatterns {
		if !had[p] {
			added = append(added, p)
		}
	}
	if len(added) > 0 {
		notes = append(notes, "Approved sources gained "+strings.Join(added, ", ")+".")
	}
	if !before.UploadsEnabled && after.UploadsEnabled {
		notes = append(notes, "ZIP uploads were switched on, so a folder with no repository behind "+
			"it may become an app.")
	}
	return strings.Join(notes, " ")
}
