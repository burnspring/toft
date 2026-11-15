package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"toft/internal/appint"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/notify"
	"toft/internal/telemetry"
)

// Service is the platform's handle on Slack. It reads its connections from the
// database on every call rather than caching them, so an administrator who has
// just re-installed their Slack app and pasted a new token does not have to
// restart the binary to find out whether it works.
//
// **A workspace is a connection row now, not a settings document.** What that
// changes here is that there may be more than one: two workspaces, or one
// workspace reached by two bots with different memberships. An app has exactly
// one, because its attachment names one; an administrator-audience notification
// goes to every connection that offers itself to the platform for that audience,
// which is the only reading of "two connections" that is not a silent choice
// between them.
type Service struct {
	db  *database.DB
	enc *crypto.Encryptor
	// envBaseURL is the BASE_URL the process was started with — the fallback
	// for when no administrator has set a public address.
	//
	// Links in a notification never come from the request that triggered it:
	// there is usually no request, and where there is one it belongs to someone
	// other than the reader. Resolved through the same
	// database.ResolvePublicBaseURL the mailer uses, so the two channels cannot
	// name two different installs.
	envBaseURL string
}

// NewService builds the poster. It never fails: an install with no connection
// configured gets a Service that reports Enabled() false and drops every post
// with a debug log.
func NewService(db *database.DB, enc *crypto.Encryptor, envBaseURL string) *Service {
	return &Service{db: db, enc: enc, envBaseURL: strings.TrimSuffix(envBaseURL, "/")}
}

// BaseURL is where links in a notification should point.
func (s *Service) BaseURL() string {
	if s == nil {
		return ""
	}
	return s.db.ResolvePublicBaseURL(s.envBaseURL)
}

// Enabled reports whether any Slack connection exists at all. Callers use it to
// decide whether to say anything to a person about it — never to decide whether
// to post, which PostAsync already handles.
func (s *Service) Enabled() bool {
	if s == nil {
		return false
	}
	conns, err := s.db.ListConnectionsOfKind(appint.IntegrationSlack)
	return err == nil && len(conns) > 0
}

// PostAsync delivers in the background and never returns an error, because no
// caller should change what it does based on whether Slack answered. The thing
// being reported has already happened; failing to report it is an operations
// problem, logged, not a user-facing one.
//
// It returns whether the message passed the gates and was dispatched — not
// whether Slack accepted it, which is decided long after this returns and is
// nobody's business up here. The one caller that reads it does so to record
// that somebody was told, because a recovery notice about a problem nobody
// heard start is worse than no recovery notice. Ignoring the result is always
// safe; it only makes that record conservative.
//
// appID names the app a message is about, and is consulted only for
// AudienceApp. An admin-audience message has no app and goes to the connections
// that offer themselves to the platform — an app's channel is for that app.
//
// This is where three of the four gates are enforced, and it is the only place
// any of them is:
//
//  1. A connection exists and offers itself to the platform.
//  2. The audience switch for this message, which is the connection's own
//     audience list.
//  3. For an app message: an administrator has attached Slack to this app, and
//     has not switched it off.
//
// The fourth gate — the app owner's own notify_failures switch — is applied by
// the caller, because it governs email in the same breath and asking an owner
// to express one wish twice is how they end up expressing it once.
func (s *Service) PostAsync(template, appID string, d notify.TemplateData) bool {
	if s == nil {
		return false
	}
	// The sender is resolved here, at the one place every platform post
	// passes, rather than passed by whoever built d — so none can forget it.
	msg, err := Render(template, s.db.ResolveSenderName(), d)
	if err != nil {
		// Not a failure. Most templates that land here are the ones with no
		// Slack rendering on purpose — the promotion outcomes, which are about
		// a person rather than an app.
		slog.Debug("no Slack rendering for this notification, skipped", "template", template)
		return false
	}
	if msg.Audience == "" {
		slog.Error("notification has no audience and was dropped; set one on its template",
			"template", template)
		return false
	}

	type target struct {
		cfg     Config
		channel string
	}
	var targets []target

	if msg.Audience == notify.AudienceApp {
		if appID == "" {
			slog.Error("an app notification arrived with no app id and was dropped", "template", template)
			return false
		}
		attachment, err := s.db.ResolveAttachment(appID, appint.IntegrationSlack)
		if err != nil {
			slog.Error("resolving an app's Slack attachment failed", "app_id", appID, "error", err)
			return false
		}
		if !attachment.Attached || !attachment.Enabled {
			slog.Debug("app has no Slack integration, notification skipped", "app_id", appID)
			return false
		}
		conn, err := s.db.GetConnection(attachment.ConnectionID)
		if err != nil || conn == nil {
			slog.Error("reading an app's Slack connection failed", "app_id", appID, "error", err)
			return false
		}
		if !conn.ForPlatform || !servesAudience(conn, msg.Audience) {
			slog.Debug("Slack notifications for this audience are off, skipped",
				"audience", msg.Audience, "template", template)
			return false
		}
		cfg, err := s.config(conn)
		if err != nil || cfg == nil {
			slog.Error("the Slack connection this app is on is unusable",
				"app_id", appID, "error", err)
			return false
		}
		targets = append(targets, target{cfg: *cfg, channel: attachment.Destination.ID})
	} else {
		conns, err := s.db.PlatformPosters(msg.Audience)
		if err != nil {
			slog.Error("reading Slack connections failed", "error", err)
			return false
		}
		for i := range conns {
			if conns[i].Kind != appint.IntegrationSlack {
				continue
			}
			cfg, err := s.config(&conns[i])
			if err != nil || cfg == nil {
				slog.Error("a Slack connection is unusable, notification skipped",
					"connection", conns[i].Name, "error", err)
				continue
			}
			targets = append(targets, target{cfg: *cfg, channel: conns[i].DefaultDestination.ID})
		}
		if len(targets) == 0 {
			slog.Debug("no Slack connection carries this audience, skipped",
				"audience", msg.Audience, "template", template)
			return false
		}
	}

	for _, t := range targets {
		if strings.TrimSpace(t.channel) == "" {
			slog.Error("a Slack connection has no default channel, notification dropped",
				"template", template)
			continue
		}
		post := msg
		post.Channel = t.channel
		cfg := t.cfg
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), PostTimeout)
			defer cancel()
			// The *channel* is never a metric attribute. It is a workspace's
			// name for a room — free text somebody typed, and an unbounded
			// label — and it is on the list ADR §31 D7 says never leaves this
			// process at all.
			start := time.Now()
			err := Post(ctx, cfg, post)
			telemetry.RecordNotify(ctx, telemetry.ChannelSlack, post.Audience,
				telemetry.Outcome(err), time.Since(start))
			if err != nil {
				// Deliberately not retried and deliberately not redirected to
				// the default channel. An app's chosen channel going wrong — the
				// bot kicked, the channel archived — must not silently move that
				// app's failure detail, which quotes build logs, into a channel
				// its people did not choose. Falling back is what an *unset*
				// channel means, and unset is a state somebody chose.
				slog.Error("posting to Slack failed",
					"template", template, "channel", post.Channel, "error", err)
				return
			}
			slog.Info("posted to Slack", "template", template, "channel", post.Channel)
		}()
	}
	return true
}

// PostTest posts to one connection's default channel synchronously and returns
// Slack's own error, because the administrator pressing the button is waiting
// for exactly that. It uses the saved connection, not a submitted form: what it
// proves is that the stored configuration works.
//
// It ignores the audience switches on purpose, for the reason mail's SendTest
// does — a diagnostic that goes quiet because notifications are off tests
// nothing and reads as a broken integration.
//
// There is no separate channel-validation call anywhere in this package. A real
// post proves the token, the scope, the channel id and the bot's membership in
// one shot; conversations.info proves a subset and costs another scope.
func (s *Service) PostTest(ctx context.Context, connID string) error {
	cfg, conn, err := s.connection(connID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(conn.DefaultDestination.ID) == "" {
		return errors.New("this connection has no default channel — choose one and save first")
	}
	msg, err := Render("test_post", s.db.ResolveSenderName(), notify.TemplateData{BaseURL: s.BaseURL()})
	if err != nil {
		return err
	}
	msg.Channel = conn.DefaultDestination.ID
	return Post(ctx, *cfg, msg)
}

// Channels lists the conversations one connection's bot can post to, for the
// two pickers: an administrator choosing the connection default, and an app's
// people choosing their app's. Both go through here so neither can see a channel
// the other cannot.
func (s *Service) Channels(ctx context.Context, connID string) ([]Channel, error) {
	cfg, _, err := s.connection(connID)
	if err != nil {
		return nil, err
	}
	return Channels(ctx, *cfg)
}

// connection reads one connection and the decrypted configuration it describes.
func (s *Service) connection(connID string) (*Config, *database.IntegrationConnection, error) {
	if s == nil {
		return nil, nil, errors.New("Slack is not available on this install")
	}
	conn, err := s.db.GetConnection(connID)
	if err != nil {
		return nil, nil, err
	}
	if conn == nil || conn.Kind != appint.IntegrationSlack {
		return nil, nil, errors.New("no such Slack connection")
	}
	cfg, err := s.config(conn)
	if err != nil {
		return nil, nil, err
	}
	return cfg, conn, nil
}

// config decrypts one connection's credential into the Config a post needs.
//
// A token that will not decrypt is an error rather than a silent post without
// one — a rotated ENCRYPTION_KEY should say so, not look like an integration
// that quietly stopped.
func (s *Service) config(conn *database.IntegrationConnection) (*Config, error) {
	if conn == nil {
		return nil, errors.New("no Slack connection")
	}
	cfg := Config{DefaultChannel: conn.DefaultDestination.ID}
	if conn.Secret != "" {
		plain, err := s.enc.Decrypt(conn.Secret)
		if err != nil {
			return nil, errors.New("the stored Slack bot token could not be decrypted — re-enter it")
		}
		var fields map[string]string
		if err := json.Unmarshal([]byte(plain), &fields); err != nil {
			return nil, fmt.Errorf("the stored Slack credential is not readable: %w", err)
		}
		cfg.Token = fields["bot_token"]
	}
	if err := cfg.validateToken(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func servesAudience(c *database.IntegrationConnection, audience string) bool {
	for _, a := range c.PlatformAudiences {
		if a == audience {
			return true
		}
	}
	return false
}
