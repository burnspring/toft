package slack

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"toft/internal/notify"
)

// The Slack rendering of the neutral notification shape.
//
// This file used to hold five templates of its own, and the fact that it held
// only five was the mechanism: **a notification was posted only if this file
// defined it**, which made the three deliberately email-only notices enforced
// by absence rather than by intention. That mechanism was right for two
// channels and does not survive eight — five notifications times seven channels
// as separate template files is thirty-five files nobody keeps in step, and a
// channel quietly rendering last month's wording is not something anybody
// reports.
//
// So the wording lives once, in internal/notify, and this file is the part that
// is genuinely Slack's: blocks, mrkdwn, and the escaping discipline that goes
// with them. What replaces absence is the `channels` field on a template plus
// `TestPersonFacingNotificationsAreMailOnly` — weaker, said out loud, and the
// price of not maintaining thirty-five files.

// SlackDetailLimit caps the failure excerpt, tighter than the 4000 bytes the
// stored event and the email body carry. A channel is a shared surface: a
// four-thousand-character paste is hostile there in a way it is not in an inbox
// somebody opted into.
const SlackDetailLimit = 1500

// Block is one Block Kit block. Only the three shapes these messages use are
// modelled — section, context, divider — because a general Block Kit builder
// would be a lot of surface for five messages.
type Block struct {
	Type string `json:"type"`
	// Text is the section body.
	Text *Text `json:"text,omitempty"`
	// Elements is the context line's parts.
	Elements []Text `json:"elements,omitempty"`
}

// Text is Slack's composition object. Always mrkdwn here; plain_text would
// force a second escaping discipline for no gain.
//
// Verbatim is always set on what these templates build. Without it Slack
// parses the text automatically: a bare `@here` or `@channel` in a display
// name, an app name or a failure line becomes a channel-wide mention, which
// esc cannot prevent because it is not markup. The explicit `<url|label>` the
// context line writes is still honoured. The cost is that a bare URL is no
// longer auto-linked.
type Text struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Verbatim bool   `json:"verbatim,omitempty"`
}

func section(format string, args ...interface{}) Block {
	return Block{Type: "section", Text: &Text{Type: "mrkdwn", Text: fmt.Sprintf(format, args...), Verbatim: true}}
}

func contextLine(format string, args ...interface{}) Block {
	return Block{Type: "context", Elements: []Text{{Type: "mrkdwn", Text: fmt.Sprintf(format, args...), Verbatim: true}}}
}

// Render builds a Slack Message from a named notification, posted as an
// install called sender ("" reads "Toft"). The Service resolves sender before
// calling this; the sites that build the data never pass it.
//
// An unknown name is not necessarily a programming error — it is also how the
// mail-only notices stay mail-only — so the caller treats it as "nothing to
// post" rather than a failure.
func Render(name, sender string, d notify.TemplateData) (Message, error) {
	sender = notify.SenderOrDefault(sender)
	msg, ok := notify.Render(name, sender, d)
	if !ok {
		return Message{}, fmt.Errorf("no Slack rendering for %q", name)
	}
	return Message{
		// Escaped like every block: Slack parses top-level text as mrkdwn,
		// and it is what notifications, screen readers and block-less clients
		// show, so a raw app name there is a live link or mention.
		Text:     esc(fallback(msg, sender)),
		Blocks:   blocks(msg, notify.Origin(sender, d.BaseURL)),
		Audience: notify.AudienceOf(name),
	}, nil
}

// blocks turns the neutral shape into Block Kit.
//
// Every string interpolated here is user-supplied — an app name, a git error, a
// build-log tail, a login from an identity provider — so every one goes through
// esc. That is not cosmetic: an unescaped `<https://evil.example|click here>`
// in any of them is a link Toft appears to have written.
//
// origin is the line naming the sender and the install's address, which the
// context line carries last.
func blocks(msg notify.Message, origin string) []Block {
	out := []Block{}
	if msg.Title != "" {
		out = append(out, section("*%s*", clipEscaped(esc(msg.Title), sectionLimit)))
	}
	if msg.Body != "" {
		out = append(out, section("%s", clipEscaped(esc(msg.Body), sectionLimit)))
	}
	for _, line := range msg.Lines {
		label := clipEscaped(esc(line.Label), 200)
		out = append(out, section("%s: `%s`", label, clipEscaped(esc(line.Value), sectionLimit-len(label))))
	}
	if excerpt := codeBlock(msg.Detail); excerpt != "" {
		out = append(out, section("%s", excerpt))
	}
	if context := contextText(msg, origin); context != "" {
		out = append(out, contextLine("%s", context))
	}
	return out
}

// contextText is the last line: the link, whatever the reader should know
// about where else this is recorded, and who sent it from where — the origin
// mail's footer carries, so a channel two installs post into can tell them
// apart in words.
func contextText(msg notify.Message, origin string) string {
	parts := []string{}
	if msg.LinkURL != "" {
		label := msg.LinkText
		if label == "" {
			label = msg.LinkURL
		}
		parts = append(parts, fmt.Sprintf("<%s|%s>", esc(msg.LinkURL), esc(label)))
	}
	if msg.Footer != "" {
		parts = append(parts, esc(msg.Footer))
	}
	if origin != "" {
		parts = append(parts, esc(origin))
	}
	return strings.Join(parts, " · ")
}

// fallback is what Slack shows in a desktop notification and in the channel
// list, so it has to stand alone.
func fallback(msg notify.Message, sender string) string {
	if msg.Fallback != "" {
		return msg.Fallback
	}
	if msg.Title != "" {
		return msg.Title
	}
	return sender
}

// esc escapes the three characters Slack's mrkdwn parser treats as markup.
//
// This is not cosmetic. Every string these templates interpolate is
// user-supplied — an app name, a git error, a build-log tail, a login from an
// identity provider — and an unescaped `<https://evil.example|click here>` in
// any of them is a link Toft appears to have written. Slack's own docs
// require these three and only these three.
func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// codeBlock renders the failure excerpt, or "" when there is none.
//
// The excerpt is escaped like everything else — a fenced block is not a safe
// harbour in mrkdwn, and `>` inside one is still markup. Backticks in the
// content are the one thing a fence cannot survive, so they are replaced rather
// than escaped: Slack has no escape for them, and the alternative is a message
// whose formatting falls apart from the middle of a log line onwards.
func codeBlock(detail string) string {
	d := strings.TrimSpace(detail)
	if d == "" {
		return ""
	}
	escaped := func(s string) string { return strings.ReplaceAll(esc(s), "```", "'''") }
	trimmed := false
	if len(d) > SlackDetailLimit {
		d = d[len(d)-SlackDetailLimit:]
		if i := strings.IndexByte(d, '\n'); i >= 0 && i < len(d)-1 {
			d = d[i+1:]
		}
		trimmed = true
	}
	// The byte cut above is on the raw text, and escaping grows it — `<` is
	// four bytes once escaped, `&` five — so 1500 bytes of markup-dense output
	// can pass Slack's 3000-character section limit, and Slack refuses the
	// whole post. It is a tail, so whole lines go from the front until the
	// escaped text fits; a single line that is still too long loses its front.
	for len(escaped(d)) > sectionLimit {
		trimmed = true
		if i := strings.IndexByte(d, '\n'); i >= 0 && i < len(d)-1 {
			d = d[i+1:]
			continue
		}
		cut := len(d) - sectionLimit/5
		for cut < len(d) && !utf8.RuneStart(d[cut]) {
			cut++
		}
		d = d[cut:]
	}
	if trimmed {
		d = "… (earlier output trimmed)\n" + d
	}
	return "```\n" + escaped(d) + "\n```"
}

// sectionLimit is what one section's text is held to: Slack refuses a post
// whose section text passes 3000 characters, and this leaves room for the
// formatting around it.
const sectionLimit = 2900

// clipEscaped cuts already-escaped text to at most n bytes on a rune boundary,
// backs up past an entity the cut would split (`&lt` without its `;`), and
// marks the cut with an ellipsis. Bytes rather than runes, so the bound holds
// however Slack counts.
func clipEscaped(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	s = s[:i]
	if amp := strings.LastIndexByte(s, '&'); amp >= 0 && !strings.Contains(s[amp:], ";") {
		s = s[:amp]
	}
	return s + "…"
}
