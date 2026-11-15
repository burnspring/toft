package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"toft/internal/appint"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/integrations"
	"toft/internal/notify"
	"toft/internal/telemetry"
)

// The platform's own notifications, on every posting kind except Slack.
//
// Slack keeps a Poster of its own because it has a rendering of its own —
// blocks, mrkdwn, and an escaping discipline that goes with them. Everything
// else takes text, so one poster serves all of them: it renders the neutral
// message once, flattens it, and hands it to whichever transports the
// connections name.
//
// **It is the same three gates the app-initiated path has, read the same way.**
// A connection that offers itself to the platform, an audience the connection
// carries, and — for an app message — an attachment that is on and points at
// that connection. Nothing here re-implements a gate; it reads
// `PlatformPosters` and `ResolveAttachment`, which is what makes "routing
// resolves in one place" true across both seams rather than only within one.
type catalogPoster struct {
	db  *database.DB
	enc *crypto.Encryptor
	// transports holds only the standalone kinds. Slack has its own Poster and
	// mail is not one at all, so the two that would need a Server are exactly
	// the two this never asks for — which is what lets it be constructed in
	// main.go beside the recorder rather than inside the API layer that used to
	// own every channel.
	transports transportRegistry
	// envBaseURL is the address the process was started with, resolved through
	// the same ResolvePublicBaseURL both other channels use so no two of them
	// can name different installs.
	envBaseURL string
}

var _ events.Poster = catalogPoster{}

// NewCatalogPoster builds the platform-notification poster for every kind that
// is not Slack and not mail.
//
// It takes what it needs rather than a Server, because the notification
// channels are constructed before the API layer: every background worker can
// notice an app break, and none of them could reach a channel only the API
// layer held.
func NewCatalogPoster(db *database.DB, enc *crypto.Encryptor, envBaseURL string) events.Poster {
	return catalogPoster{
		db: db, enc: enc,
		transports: standaloneTransports(),
		envBaseURL: strings.TrimSuffix(envBaseURL, "/"),
	}
}

// PostTimeout bounds one platform post.
const catalogPostTimeout = 30 * time.Second

// BaseURL is where links in a notification should point.
func (c catalogPoster) BaseURL() string { return c.db.ResolvePublicBaseURL(c.envBaseURL) }

// PostAsync delivers in the background and never returns an error, because no
// caller should change what it does based on whether a third party answered.
// The thing being reported has already happened; failing to report it is an
// operations problem, logged, not a user-facing one.
//
// It reports whether the message passed the gates and was dispatched — not
// whether anybody accepted it, which is decided long after this returns.
func (c catalogPoster) PostAsync(template, appID string, d notify.TemplateData) bool {
	// The sender is resolved here, where every platform post passes, rather
	// than passed by the sites that built d — so none of them can forget it.
	sender := c.db.ResolveSenderName()
	msg, ok := notify.Render(template, sender, d)
	if !ok {
		// Not a failure. Most names that land here are the ones with no neutral
		// rendering on purpose — the person-facing notices, which are about
		// somebody rather than about an app.
		slog.Debug("no posted rendering for this notification, skipped", "template", template)
		return false
	}
	audience := notify.AudienceOf(template)
	if audience == "" {
		slog.Error("notification has no audience and was dropped; declare one on its template",
			"template", template)
		return false
	}

	conns, err := c.db.PlatformPosters(audience)
	if err != nil {
		slog.Error("reading the connections that carry notifications failed", "error", err)
		return false
	}

	dispatched := false
	for i := range conns {
		conn := conns[i]
		// Slack has its own Poster and its own rendering. Asking both would
		// post twice.
		if conn.Kind == appint.IntegrationSlack || conn.Kind == appint.IntegrationEmail {
			continue
		}
		transport := c.transports[conn.Kind]
		if transport == nil {
			continue
		}

		destination := conn.DefaultDestination
		if audience == notify.AudienceApp {
			if appID == "" {
				slog.Error("an app notification arrived with no app id and was dropped",
					"template", template)
				continue
			}
			attachment, err := c.db.ResolveAttachment(appID, conn.Kind)
			if err != nil {
				slog.Error("resolving an app's attachment failed",
					"app_id", appID, "kind", conn.Kind, "error", err)
				continue
			}
			// An app's own destination is for that app, so a notification about
			// it goes where its own messages go — and an app with no attachment
			// on this connection, or one switched off, is told nothing through
			// it. The connection's "Apps can send" switch (for_apps) is not a
			// gate here: it governs what apps send, and Toft's own notices are
			// gated by for_platform and the audience, which PlatformPosters has
			// already applied — the rule Slack's poster keeps too.
			if !attachment.Attached || !attachment.Enabled || attachment.ConnectionID != conn.ID {
				continue
			}
			destination = attachment.Destination
		}

		target, err := connectionTarget(c.db, c.enc, conn.ID, destination)
		if err != nil {
			slog.Error("a connection that carries notifications is unusable",
				"connection", conn.Name, "error", err)
			continue
		}
		send := flatten(conn.Kind, msg, d, sender)

		dispatched = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), catalogPostTimeout)
			defer cancel()
			start := time.Now()
			_, err := transport.Send(ctx, target, send)
			// The channel is the *kind*, which is a compiled-in name from the
			// catalog and therefore a bounded label. The **destination** is
			// never an attribute: it is somebody's name for a room or a folder
			// — free text, and unbounded — and it is on the list ADR §31 D7
			// says never leaves this process at all.
			telemetry.RecordNotify(ctx, conn.Kind, audience,
				telemetry.Outcome(err), time.Since(start))
			if err != nil {
				// Deliberately not retried and deliberately not redirected to
				// another destination. A chosen destination going wrong must not
				// silently move an app's failure detail, which quotes build
				// logs, somewhere its people did not choose.
				slog.Error("posting a notification failed",
					"template", template, "kind", conn.Kind, "error", err)
				return
			}
			slog.Info("posted a notification", "template", template, "kind", conn.Kind)
		}()
	}
	return dispatched
}

// flatten turns the neutral message into the one shape every text transport
// takes.
//
// It is the lowest common denominator on purpose, and it is *only* used for the
// channels that have no rendering of their own: Slack has one, and the day a
// Teams card wants a fact set rather than a paragraph, that transport grows a
// renderer and this function stops being what it reads.
//
// sender is the install's name, or "Toft": the card's sender and the link's
// label, because this is the platform speaking and the install is its name.
func flatten(kind string, msg notify.Message, d notify.TemplateData, sender string) integrations.Send {
	sender = notify.SenderOrDefault(sender)
	var b strings.Builder
	if msg.Body != "" {
		b.WriteString(msg.Body)
	}
	for _, line := range msg.Lines {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line.Label + ": " + line.Value)
	}
	if excerpt := strings.TrimSpace(msg.Detail); excerpt != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(excerpt)
	}
	if msg.LinkURL != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		label := msg.LinkText
		if label == "" {
			label = "Open " + sender
		}
		b.WriteString(label + ": " + msg.LinkURL)
	}
	if msg.Footer != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("(" + msg.Footer + ")")
	}

	send := integrations.Send{
		// Every kind's *first* action is the one a platform notification uses,
		// which for PagerDuty is `trigger` — this is the platform saying
		// something is wrong, and there is nothing here that closes an incident
		// (see internal/integrations/pagerduty for why that is a decision this
		// change does not take).
		Action:   platformAction(kind),
		Title:    msg.Title,
		Text:     b.String(),
		Severity: msg.Severity,
		// The platform is the sender, and it says so. This is the one place a
		// message legitimately speaks in the platform's voice, which is why the
		// app-initiated path can never reach it — now under the install's name
		// when it has one.
		AppName:      sender,
		AppURL:       d.BaseURL,
		Platform:     sender,
		FromPlatform: true,
	}
	if msg.Severity == "" {
		send.Severity = "info"
	}
	if kind == appint.IntegrationPagerDuty {
		// A dedup key so a problem that repeats is one incident. It is the
		// platform's own namespace rather than an app's, which is why it does
		// not go through pagerduty.DedupKey.
		send.DedupKey = "toft:" + strings.ToLower(strings.ReplaceAll(msg.Title, " ", "-"))
	}
	return send
}

func platformAction(kind string) string {
	k := appint.Lookup(kind)
	if k == nil {
		return "post"
	}
	if a := k.TextAction(); a != nil {
		return a.Name
	}
	return "post"
}
