package api

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"toft/internal/database"
)

// The security.txt half of Admin → Settings (RFC 9116).
//
// Four fields, and the rules between them exist for one reason: an expired or
// contentless security.txt is worse than none. It is a published promise that
// somebody is reading a mailbox, and the only failure mode that matters here
// is the one where it stays correct for a year and then names a person who
// left. So the contact is what turns the file on, the expiry is required
// beside it, and a date in the past is refused rather than stored.

// securityTxtLanguageRe is one BCP 47 tag, loosely: a primary subtag and any
// number of hyphen-separated subtags. Loose deliberately — the RFC points at
// the whole of BCP 47 and this is a hint to a human reader, not something the
// platform acts on. What it refuses is a sentence typed into the box.
var securityTxtLanguageRe = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)

// maxSecurityTxtExpiryDays bounds how far ahead the expiry may be set.
//
// The RFC recommends less than a year and gives no ceiling. This one is a
// little over two, which is not a standard's opinion but this platform's: a
// date further out than that is an administrator declining to be reminded, and
// the reminder is the entire value of the field.
const maxSecurityTxtExpiryDays = 800

// applySecurityTxtSettings validates and applies the four fields, returning a
// refusal or "".
//
// It reads the request *and* the settings as this request leaves them, so one
// save may set a contact and its expiry together — checking the expiry against
// the stored value would refuse the only request that can ever turn this on.
func applySecurityTxtSettings(req *updateSettingsRequest,
	settings *database.PlatformSettings, warning *string) string {

	hadContact := settings.SecurityTxtConfigured()

	if req.SecurityTxtContact != nil {
		v := strings.TrimSpace(*req.SecurityTxtContact)
		if v != "" {
			if why := securityTxtContactError(v); why != "" {
				return why
			}
		}
		settings.SecurityTxtContact = v
	}

	if req.SecurityTxtExpires != nil {
		v := strings.TrimSpace(*req.SecurityTxtExpires)
		if v != "" {
			d, err := time.Parse("2006-01-02", v)
			if err != nil {
				return "security_txt_expires must be a date written as YYYY-MM-DD."
			}
			// Compared against today in UTC, which is the clock the file is
			// served in. A date that is already gone is refused rather than
			// stored: this field exists to make staleness visible, and an
			// administrator allowed to type one in the past has been given a
			// control that does nothing.
			today := time.Now().UTC().Truncate(24 * time.Hour)
			if !d.After(today) {
				return "security_txt_expires must be a date in the future. This is the field that " +
					"makes a stale security.txt visible, and one that has already passed publishes " +
					"a contact nobody is promising to read."
			}
			if d.Sub(today) > maxSecurityTxtExpiryDays*24*time.Hour {
				return "security_txt_expires must be within " + strconv.Itoa(maxSecurityTxtExpiryDays) +
					" days. RFC 9116 asks for less than a year, and a date further out than this is " +
					"a way of declining to be reminded — which is the one thing the field is for."
			}
		}
		settings.SecurityTxtExpires = v
	}

	if req.SecurityTxtPolicy != nil {
		v := strings.TrimSpace(*req.SecurityTxtPolicy)
		if v != "" {
			u, err := url.Parse(v)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return "security_txt_policy must be an https:// address, or empty. It is published " +
					"to whoever fetches the file, so it has to be somewhere they can reach."
			}
		}
		settings.SecurityTxtPolicy = v
	}

	if req.SecurityTxtLanguages != nil {
		v := normalizeSecurityTxtLanguages(*req.SecurityTxtLanguages)
		for _, tag := range strings.Split(v, ", ") {
			if tag == "" {
				continue
			}
			if !securityTxtLanguageRe.MatchString(tag) {
				return "security_txt_preferred_languages must be language tags separated by " +
					"commas — en, fr, de-CH — or empty. “" + tag + "” is not one."
			}
		}
		settings.SecurityTxtLanguages = v
	}

	// The pairing, checked after everything is applied rather than inside the
	// contact's branch: an administrator may set either field first, and the
	// rule is about the state this request leaves behind.
	//
	// **Refused, not defaulted.** Publishing a file with no expiry, or
	// inventing one a year out on somebody's behalf, would both be the
	// platform making a commitment nobody made.
	if settings.SecurityTxtConfigured() && strings.TrimSpace(settings.SecurityTxtExpires) == "" {
		return "A published security.txt needs an expiry date: RFC 9116 requires the field, and it " +
			"is what stops this file quietly naming a contact nobody reads. Set security_txt_expires, " +
			"or clear the contact to stop publishing the file."
	}

	// Both directions of the switch said out loud, because what changes is
	// what the anonymous internet can fetch from this install.
	switch {
	case !hadContact && settings.SecurityTxtConfigured() && *warning == "":
		*warning = "This install now publishes /.well-known/security.txt to anyone who asks, on " +
			"both the platform address and the address apps are served from. Whoever is named in " +
			"it should know the reports are coming."
	case hadContact && !settings.SecurityTxtConfigured() && *warning == "":
		*warning = "/.well-known/security.txt is no longer published — the path answers 404 on " +
			"both addresses from now. Anyone who bookmarked the contact has nothing to fall back on."
	}
	return ""
}

// securityTxtContactError checks a contact URI, or returns "".
//
// RFC 9116 wants a URI and singles out `mailto:` and `tel:`; a web form is
// ordinary too, and has to be https for the same reason the policy URL does.
// Anything else is refused by name rather than published: a bare email address
// with no scheme is the mistake everybody makes here, and a file carrying one
// fails every validator that reads it.
func securityTxtContactError(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" {
		return "security_txt_contact must be a URI — mailto:security@example.com, " +
			"https://example.com/security, or tel:+1-201-555-0123. A bare email address is not one."
	}
	switch strings.ToLower(u.Scheme) {
	case "mailto", "tel":
		if u.Opaque == "" && u.Path == "" {
			return "security_txt_contact is missing the address after " + u.Scheme + ":."
		}
		return ""
	case "https":
		if u.Host == "" {
			return "security_txt_contact is missing a hostname."
		}
		return ""
	}
	return "security_txt_contact must be mailto:, https:// or tel:. " +
		"“" + u.Scheme + ":” is published to whoever fetches the file, and has to be something " +
		"they can act on."
}
