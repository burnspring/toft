package mail

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	"toft/internal/database"
	"toft/internal/notify"
)

// App-initiated mail: a message an *app* wrote, not the platform.
//
// Everything here exists to serve one rule. **An app never speaks in the
// platform's voice.** The reasoning is ADR §19's, applied to a message instead
// of a hostname: if app authors can be indistinguishable from Toft, then
// "Toft says" stops meaning anything, and the first phishing mail that
// matters will be the one that was technically legitimate.
//
// So the app supplies the message and the platform supplies the identity. The
// display name says which app sent it, the frame says an application did, the
// envelope address stays the install's because SPF requires it, and none of
// those three is reachable from anything app code sent.

// AppDisplaySuffix is appended to every app-initiated sender name: " (via
// <install name>)", or " (via Toft)" on an install with no name.
//
// Fixed per app and set per install. The reason it is not configurable — an app
// that could drop it could pass for the platform in one field change — is about
// *who* can change it, and no app can: the name is an administrator's setting,
// passed by the platform to BuildAppMessage and never taken from anything app
// code sent. What the administrator chooses is the word, the same word every
// other message from this install carries.
func AppDisplaySuffix(installName string) string {
	return " (via " + senderOrDefault(installName) + ")"
}

// senderOrDefault is the install's name, or "Toft" when it has none.
func senderOrDefault(installName string) string {
	if v := strings.TrimSpace(installName); v != "" {
		return v
	}
	return database.DefaultSenderName
}

// appNameLimit bounds the display name. Long names are not an attack, they are
// a header that wraps badly — but the truncation is here rather than at the
// call site so that nothing can route around it.
const appNameLimit = 64

// BuildAppMessage assembles what an app asked to send into what actually goes
// out. It is the only constructor for app-initiated mail.
//
// installName is the install's name as ResolveSenderName answers it — never
// anything from the app's request. appName is the app's own name, replyTo the
// address from the app's integration row (empty falls back to the install's),
// and appURL the link back to the app.
//
// The app's name is not shortened to make room for a long install name. A From
// of ~140 characters is legal, a narrow inbox list cuts the suffix first, and
// the frame inside names the install again; trimming the app's name to protect
// the install's would make the more specific of the two the one that loses.
func BuildAppMessage(installName, appName, appURL, replyTo, subject, text, htmlBody string,
	to, cc, bcc []string, attachments []Attachment) Message {

	display := SanitizeDisplayName(appName)
	platform := senderOrDefault(installName)
	return Message{
		To:      to,
		CC:      cc,
		BCC:     bcc,
		Subject: SanitizeSubject(subject),
		// FromName only. The address stays the install's — SPF is checked
		// against the envelope sender, and an app-chosen address would fail it
		// on every install that publishes a record.
		FromName:    display + AppDisplaySuffix(platform),
		ReplyTo:     replyTo,
		Text:        frameText(text, display, appURL, platform),
		HTML:        frameHTML(htmlBody, display, appURL, platform),
		Attachments: attachments,
		// The audience app-initiated mail belongs to. Not AudienceApp: that
		// switch means "stop sending app authors platform chatter about their
		// apps", and an administrator who throws it is not asking to shut down
		// a business process that mails a finance team every Monday.
		Audience: notify.AudienceAppOutbound,
	}
}

// SendForApp delivers an app-initiated message **synchronously** and returns
// the relay's own error.
//
// Synchronous on purpose, and it is not a hole in "a send never fails the
// request that triggered it": the request that triggered this one was answered
// with a 202 long before this runs. The caller is a queue worker whose entire
// job is to find out what happened and write it in the ledger, and SendAsync —
// which returns nothing, by design — cannot tell it.
//
// It does not consult the audience switches. Those govern platform chatter;
// this message's gates are the install's integrations.email switch and the
// per-app attachment, and appint.Check applied them before the send was
// accepted and again immediately before this call.
func (s *Service) SendForApp(ctx context.Context, msg Message) error {
	if s == nil {
		return errors.New("email is not configured on this install")
	}
	_, cfg, err := s.load()
	if err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("no mail relay is configured on this install — an administrator " +
			"sets one up in Settings › Email")
	}
	return Send(ctx, *cfg, msg)
}

// SanitizeDisplayName makes an app's name safe to put in a header.
//
// An app named with a CR/LF is not an address-header injection, and the reason
// it is not is this function rather than anybody's care at the call site.
func SanitizeDisplayName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, name)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		// Never empty: a blank display name beside "(via <install>)" reads as
		// the platform itself, which is the one thing the frame prevents.
		cleaned = "An application"
	}
	if len(cleaned) > appNameLimit {
		cleaned = strings.TrimSpace(cleaned[:appNameLimit])
	}
	return cleaned
}

// SanitizeSubject strips what a header cannot carry. Length is enforced by the
// handler, which refuses rather than truncates — losing the end of a subject
// silently is how a report arrives titled "Weekly reconciliation for".
func SanitizeSubject(subject string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, subject)), " ")
}

// ValidateRecipient parses and normalizes one address, rejecting anything that
// could carry a header past it.
func ValidateRecipient(addr string) (string, error) {
	trimmed := strings.TrimSpace(addr)
	if strings.ContainsAny(trimmed, "\r\n") {
		return "", fmt.Errorf("%q contains a line break", addr)
	}
	parsed, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid email address", addr)
	}
	// Re-rendered from the parse rather than passed through, which neutralizes
	// anything hiding in a display name.
	return parsed.Address, nil
}

// frameText appends the plain-text half of the frame.
//
// The plain-text half is the one that matters most: it is the half no styling
// can paint over, which is why `text` is required even when an app supplies
// HTML.
//
// platform is the install's name, or "Toft": the frame's job — an application
// wrote this, the platform did not — is unchanged, and the word naming the
// platform is the install's own.
func frameText(body, appName, appURL, platform string) string {
	frame := "\n\n--\nSent by " + appName + ", an application hosted on " + platform + ".\n" +
		"This message was written by that application, not by " + platform + " itself."
	if appURL != "" {
		frame += "\n" + appURL
	}
	return strings.TrimRight(body, "\n") + frame
}

// frameHTML sanitizes the app's HTML and wraps it in the frame. Empty in,
// empty out: an app that sent no HTML gets a plain-text message rather than a
// frame around nothing.
//
// The mark is drawn only on an install with no name, for the reason the
// platform frame drops it: Toft's mark beside another name names two senders.
func frameHTML(body, appName, appURL, platform string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	link := ""
	if appURL != "" {
		link = `<br><a href="` + html.EscapeString(appURL) + `">` + html.EscapeString(appURL) + `</a>`
	}
	mark := ""
	if platform == database.DefaultSenderName {
		mark = markImg + `&nbsp; `
	}
	return `<div>` + SanitizeHTML(body) + `</div>` +
		`<hr style="margin-top:24px;border:none;border-top:1px solid #d4d4d8">` +
		`<div style="color:#71717a;font-size:12px">` + mark + `Sent by ` +
		html.EscapeString(appName) +
		`, an application hosted on ` + html.EscapeString(platform) + `. This message was written by that ` +
		`application, not by ` + html.EscapeString(platform) + ` itself.` + link + `</div>`
}

// ------------------------------------------------------------- sanitization

// Element and attribute allow-lists.
//
// Sanitizing is not a contradiction of "the frame is not app-editable" — it is
// what makes it true. An app that can ship <style> can hide the frame, restyle
// the message as a Toft notice, and absolutely-position its own header
// over the attribution. A separate frame template is worth nothing if the
// content can paint over it.
var allowedElements = map[string]bool{
	"p": true, "br": true, "div": true, "span": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true,
	"strong": true, "b": true, "em": true, "i": true, "u": true, "s": true,
	"small": true, "sub": true, "sup": true, "code": true, "pre": true,
	"blockquote": true, "a": true, "img": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true,
	"tr": true, "th": true, "td": true, "caption": true,
}

// Elements whose *contents* go too. Dropping the tag but keeping the text of a
// <script> would put the script body in the message as visible text, which is
// noise; for <style> it would put the rules in as text, which is worse.
var strippedSubtrees = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true,
	"embed": true, "form": true, "head": true, "title": true,
	"noscript": true, "template": true, "svg": true, "math": true,
}

var allowedAttrs = map[string]map[string]bool{
	"a":   {"href": true, "title": true},
	"img": {"src": true, "alt": true, "width": true, "height": true},
	"td":  {"colspan": true, "rowspan": true, "align": true},
	"th":  {"colspan": true, "rowspan": true, "align": true},
}

// A small safe subset of inline CSS. Inline styles matter in email — most
// clients strip <style> blocks — so dropping them entirely would make app mail
// unusable for the reports this feature exists to send. What is excluded is
// everything that can move content out of its own box or reach the network:
// position, z-index, negative offsets, and url().
var allowedStyleProps = map[string]bool{
	"color": true, "background-color": true, "background": true,
	"font-size": true, "font-weight": true, "font-style": true,
	"font-family": true, "text-align": true, "text-decoration": true,
	"padding": true, "padding-top": true, "padding-bottom": true,
	"padding-left": true, "padding-right": true,
	"margin": true, "margin-top": true, "margin-bottom": true,
	"border": true, "border-top": true, "border-bottom": true,
	"border-left": true, "border-right": true, "border-collapse": true,
	"width": true, "max-width": true, "height": true, "line-height": true,
	"vertical-align": true, "white-space": true,
}

// SanitizeHTML reduces an app's HTML to the allow-list above.
//
// Written against the tokenizer rather than the tree parser on purpose: the
// tree parser would helpfully reconstruct a well-formed document around
// whatever it was given, which means inventing <html> and <body> for a fragment
// that is about to be embedded in one.
func SanitizeHTML(input string) string {
	var out strings.Builder
	var open []string
	skipDepth := 0
	var skipTag string

	z := html.NewTokenizer(strings.NewReader(input))
	for {
		switch z.Next() {
		case html.ErrorToken:
			// Close anything still open so the fragment cannot leak structure
			// into the frame that follows it.
			for i := len(open) - 1; i >= 0; i-- {
				out.WriteString("</" + open[i] + ">")
			}
			return out.String()

		case html.TextToken:
			if skipDepth == 0 {
				out.WriteString(html.EscapeString(string(z.Text())))
			}

		case html.StartTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			if skipDepth > 0 {
				if tag == skipTag {
					skipDepth++
				}
				continue
			}
			if strippedSubtrees[tag] {
				skipDepth = 1
				skipTag = tag
				continue
			}
			if !allowedElements[tag] {
				// Unknown element, known-safe contents: drop the tag and keep
				// what it wrapped.
				continue
			}
			out.WriteString("<" + tag + renderAttrs(z, tag, hasAttr))
			if isVoid(tag) {
				out.WriteString(">")
			} else {
				out.WriteString(">")
				open = append(open, tag)
			}

		case html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			if skipDepth > 0 || strippedSubtrees[tag] || !allowedElements[tag] {
				continue
			}
			out.WriteString("<" + tag + renderAttrs(z, tag, hasAttr) + ">")

		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if skipDepth > 0 {
				if tag == skipTag {
					skipDepth--
					if skipDepth == 0 {
						skipTag = ""
					}
				}
				continue
			}
			if !allowedElements[tag] || isVoid(tag) {
				continue
			}
			// Only close a tag this sanitizer actually opened, so a stray
			// </div> cannot close the frame's own wrapper.
			for i := len(open) - 1; i >= 0; i-- {
				if open[i] == tag {
					for j := len(open) - 1; j >= i; j-- {
						out.WriteString("</" + open[j] + ">")
					}
					open = open[:i]
					break
				}
			}

		case html.CommentToken, html.DoctypeToken:
			// Dropped. A conditional comment is a rendering instruction to one
			// mail client and not something an app needs.
		}
	}
}

func isVoid(tag string) bool {
	return tag == "br" || tag == "hr" || tag == "img"
}

func renderAttrs(z *html.Tokenizer, tag string, hasAttr bool) string {
	if !hasAttr {
		return ""
	}
	allowed := allowedAttrs[tag]
	var out strings.Builder
	for {
		key, val, more := z.TagAttr()
		name := strings.ToLower(string(key))
		value := string(val)

		switch {
		case name == "style":
			if safe := safeStyle(value); safe != "" {
				out.WriteString(` style="` + html.EscapeString(safe) + `"`)
			}
		case strings.HasPrefix(name, "on"):
			// Every event handler, without enumerating them: the list grows
			// with the web platform and an allow-list of "not onclick" is a
			// list that is wrong next year.
		case allowed != nil && allowed[name]:
			if v, ok := safeAttrValue(tag, name, value); ok {
				out.WriteString(` ` + name + `="` + html.EscapeString(v) + `"`)
			}
		}
		if !more {
			return out.String()
		}
	}
}

func safeAttrValue(tag, name, value string) (string, bool) {
	v := strings.TrimSpace(value)
	switch name {
	case "href":
		lower := strings.ToLower(v)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") ||
			strings.HasPrefix(lower, "mailto:") {
			return v, true
		}
		return "", false
	case "src":
		lower := strings.ToLower(v)
		// https or an attachment reference. Plain http is excluded because a
		// remote image over cleartext is a tracking pixel with no transport
		// security, in a message the recipient did not ask for.
		if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "cid:") {
			return v, true
		}
		return "", false
	}
	if strings.ContainsAny(v, "\r\n") {
		return "", false
	}
	return v, true
}

// negativeLength is a minus before a number anywhere in a style value.
var negativeLength = regexp.MustCompile(`-\s*[0-9.]`)

// functionCall is every function a style value calls, by name (lower-cased).
var functionCall = regexp.MustCompile(`([a-z-]*)\(`)

// colourFunctions are the only functions a style value may call. Arithmetic is
// refused whole — calc, min, max, clamp — because `calc(0px - (400px))` spells
// a negative offset that no pattern for a written minus can see; var, env and
// attr are refused with it.
var colourFunctions = map[string]bool{"rgb": true, "rgba": true, "hsl": true, "hsla": true}

func callsOnlyColourFunctions(lower string) bool {
	for _, m := range functionCall.FindAllStringSubmatch(lower, -1) {
		if !colourFunctions[m[1]] {
			return false
		}
	}
	return true
}

func safeStyle(value string) string {
	var kept []string
	for _, decl := range strings.Split(value, ";") {
		parts := strings.SplitN(decl, ":", 2)
		if len(parts) != 2 {
			continue
		}
		prop := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		lower := strings.ToLower(val)
		if !allowedStyleProps[prop] {
			continue
		}
		// url() reaches the network, expression() is script on old clients, and
		// a negative offset moves content out of its own box — in any token of
		// the value, not only the first: `margin:0 0 -400px` pulls the frame up
		// under the app's block as surely as `margin-top:-400px` does. A
		// backslash is refused with it, because a CSS escape (`\2d`) spells
		// the minus without writing one. Arithmetic is refused, not only a
		// written minus: a value may call a colour function and nothing else,
		// because `calc(0px - (400px))` computes the negative offset the
		// pattern cannot see.
		if strings.Contains(lower, "url(") || strings.Contains(lower, "expression") ||
			strings.Contains(lower, "javascript:") || negativeLength.MatchString(val) ||
			strings.Contains(val, `\`) || !callsOnlyColourFunctions(lower) {
			continue
		}
		if strings.ContainsAny(val, "{}<>\r\n") {
			continue
		}
		kept = append(kept, prop+":"+val)
	}
	return strings.Join(kept, ";")
}
