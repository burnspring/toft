package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"toft/internal/appint"
	"toft/internal/builder"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/integrations"
	"toft/internal/integrations/discord"
	"toft/internal/integrations/google"
	"toft/internal/integrations/msfiles"
	"toft/internal/integrations/pagerduty"
	"toft/internal/integrations/teams"
	"toft/internal/integrations/teamsbot"
	"toft/internal/integrations/webhook"
	mailer "toft/internal/mail"
	"toft/internal/slack"
)

// Wiring the send queue to the transports.
//
// This is the only file that knows about all of them, which is the point:
// appint holds the gates and the ledger and imports none, and no transport
// knows about another. It used to be a `switch` with an arm per integration and
// a comment claiming a third would be one more arm and nothing else — a claim
// that was never tested, because the third integration was synchronous and
// never came through here at all (see internal/appint's package comment).
//
// It is a **registry** now, built once at startup: `map[string]integrations.
// Transport`, one entry per catalog kind, and a kind in `appint.Known` with no
// entry is a startup failure rather than a 403 nobody can explain. That is what
// makes "the list is the specification" true of transports and not only of
// descriptors.

// transportRegistry maps a catalog kind to the thing that delivers it.
type transportRegistry map[string]integrations.Transport

// buildTransports assembles the registry.
//
// Slack and mail are adapters over the services that already exist rather than
// new packages: what changed is the seam they are reached through, not the code
// that renders blocks or builds MIME.
func (s *Server) buildTransports() transportRegistry {
	reg := standaloneTransports()
	// The two that are adapters over services this install may or may not have.
	// Everything else is a leaf and is built by the function above, which is
	// what lets the platform-notification poster hold a registry without
	// holding a Server.
	reg[appint.IntegrationSlack] = slack.Transport{}
	reg[appint.IntegrationEmail] = mailTransport{s}
	return reg
}

// standaloneTransports are the kinds that need nothing from this process: no
// database handle, no encryptor, no service. Every one is a leaf that can be
// exercised against a recorded fixture with no platform underneath it, which is
// the property that makes them testable and the reason they are separable here.
func standaloneTransports() transportRegistry {
	return transportRegistry{
		appint.IntegrationWebhook: webhook.New(),

		// The Microsoft trio. SharePoint and OneDrive are one mechanism twice —
		// the same tenant credential and the same drive API, differing only in
		// which drive — and Teams shares nothing with them but the vendor's
		// name, because it is a webhook.
		appint.IntegrationSharePoint: msfiles.SharePoint(),
		appint.IntegrationOneDrive:   msfiles.OneDrive(),
		appint.IntegrationTeams:      teams.New(),
		appint.IntegrationTeamsBot:   teamsbot.New(),

		// Google, Discord and PagerDuty. Independent of each other and of
		// everything above.
		appint.IntegrationGoogleDrive: google.Drive{},
		appint.IntegrationGoogleChat:  google.Chat{},
		appint.IntegrationDiscord:     discord.New(),
		appint.IntegrationPagerDuty:   pagerduty.New(),
	}
}

// startAppIntegrations builds the queue, sweeps up what a previous process left
// behind, and starts the workers.
//
// The sweep is what stops the ledger lying. The queue is in memory, so a
// restart loses whatever was in it; without this those rows would say `queued`
// forever and an owner would have no way to tell "still going" from "never
// happened".
func (s *Server) startAppIntegrations() {
	if s.appSender != nil {
		return
	}
	s.transports = s.buildTransports()

	started := time.Now().UTC()
	if n, err := s.db.AbandonQueuedSendsBefore(started); err != nil {
		slog.Error("sweeping up sends a previous process left queued failed", "error", err)
	} else if n > 0 {
		slog.Warn("sends were left queued by a previous process and are recorded as abandoned",
			"count", n)
	}

	s.appSender = appint.NewSender(s.db, s.deliverAppSend)
	s.appSender.OnFailure(s.recordSendFailure)
	s.appSender.Start()
}

// stopAppIntegrations drains what has already been accepted.
func (s *Server) stopAppIntegrations() {
	s.appSender.Stop()
}

// deliverAppSend hands one job to the right transport.
//
// The three things it does that no transport should have to: resolve the
// connection, decrypt its credential, and record a placement. The last is why
// the transport returns a Result rather than only an error — an object that now
// exists in somebody else's system is a fact this platform has to write down
// before it forgets which app put it there.
func (s *Server) deliverAppSend(ctx context.Context, j appint.Job) error {
	kind := appint.Lookup(j.Send.Kind)
	if kind == nil {
		return fmt.Errorf("%q is not an integration this build offers", j.Send.Kind)
	}
	transport := s.transports[j.Send.Kind]
	if transport == nil {
		return fmt.Errorf("no transport for %q", j.Send.Kind)
	}

	target, err := s.connectionTarget(j.ConnectionID, j.Destination)
	if err != nil {
		return err
	}

	send := integrations.Send{
		Action:   j.Send.Action,
		Text:     j.Payload.Text,
		Title:    j.Payload.Title,
		Comment:  j.Payload.Comment,
		Subject:  j.Payload.Subject,
		HTML:     j.Payload.HTML,
		To:       j.Payload.To,
		CC:       j.Payload.CC,
		BCC:      j.Payload.BCC,
		Files:    j.Payload.Files,
		Severity: j.Payload.Severity,
		DedupKey: j.Payload.DedupKey,
		AppID:    j.Send.AppID,
		AppName:  j.AppName,
		AppURL:   s.appURL(j.BaseURL, j.Send.AppID, j.AppSlug),
		// The install's name for the attribution, from the setting at
		// delivery — never from the job, which app code shaped.
		Platform: s.db.ResolveSenderName(),
		SendID:   j.Send.ID,
	}
	if len(j.Payload.Files) > 0 {
		send.File = &j.Payload.Files[0]
	}

	result, err := transport.Send(ctx, target, send)
	if err != nil {
		return err
	}
	if result.Placement != nil {
		s.recordPlacement(j, target, *result.Placement)
	}
	return nil
}

// connectionTarget resolves and decrypts one connection.
//
// The one place a stored integration credential becomes plaintext. Below this
// line a transport is handed a map of strings and has no way to reach a crypto
// record, which is the same discipline the Slack bot token has always been
// under and now applies to eight kinds instead of one.
func (s *Server) connectionTarget(connID string, dest database.Destination) (integrations.Target, error) {
	return connectionTarget(s.db, s.encryptor, connID, dest)
}

func connectionTarget(db *database.DB, enc *crypto.Encryptor, connID string,
	dest database.Destination) (integrations.Target, error) {

	conn, err := db.GetConnection(connID)
	if err != nil {
		return integrations.Target{}, err
	}
	if conn == nil {
		return integrations.Target{}, errors.New("the connection this send was accepted on no longer exists")
	}
	creds, err := connectionCredentials(enc, conn)
	if err != nil {
		return integrations.Target{}, err
	}
	if dest.ID == "" && dest.Label == "" {
		dest = conn.DefaultDestination
	}
	places, err := db.ListPlaces(conn.ID)
	if err != nil {
		return integrations.Target{}, err
	}
	return integrations.Target{Connection: *conn, Credentials: creds, Destination: dest,
		Places: places}, nil
}

// connectionCredentials decrypts a connection's secret fields.
//
// One record holding a JSON document of the kind's secret fields, rather than a
// column per field: eight kinds' credentials as columns would be a schema
// change per kind, which is exactly the cost the catalog exists to remove.
func (s *Server) connectionCredentials(conn *database.IntegrationConnection) (map[string]string, error) {
	return connectionCredentials(s.encryptor, conn)
}

func connectionCredentials(enc *crypto.Encryptor,
	conn *database.IntegrationConnection) (map[string]string, error) {

	out := map[string]string{}
	if conn == nil || conn.Secret == "" {
		return out, nil
	}
	if enc == nil {
		return nil, errors.New("this install cannot read stored credentials")
	}
	plain, err := enc.Decrypt(conn.Secret)
	if err != nil {
		return nil, errors.New("the stored credential for this connection could not be decrypted — re-enter it")
	}
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		return nil, errors.New("the stored credential for this connection is not readable")
	}
	return out, nil
}

// sealCredentials is the other direction: the fields an administrator typed,
// as one ciphertext.
func (s *Server) sealCredentials(fields map[string]string) (string, error) {
	if len(fields) == 0 {
		return "", nil
	}
	if s.encryptor == nil {
		return "", errors.New("this install cannot store credentials")
	}
	blob, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return s.encryptor.Encrypt(string(blob))
}

// recordPlacement writes down that an app put an object somewhere, and stamps
// the send row so retention keeps it.
//
// Failing to record it does not fail the send: the message did go out, and
// telling somebody it did not would be a worse lie than a missing row. It is
// logged loudly, because a placement nobody wrote down is exactly the residue
// this feature exists to make visible.
func (s *Server) recordPlacement(j appint.Job, target integrations.Target, p integrations.Placement) {
	row := placementRow(j.Send.Kind, target, p)
	row.AppID = j.Send.AppID
	// The app's name at the time, as plain text with no foreign key: the row
	// outlives the app, and app_events set that precedent for the same reason.
	row.AppName = j.AppName
	row.SendID = j.Send.ID
	if err := s.db.InsertPlacement(row); err != nil {
		slog.Error("recording what an app left in another system failed",
			"send_id", j.Send.ID, "app_id", j.Send.AppID, "kind", j.Send.Kind, "error", err)
		return
	}
	if err := s.db.MarkSendPlacement(j.Send.ID, row.ID); err != nil {
		slog.Error("stamping a send with its placement failed",
			"send_id", j.Send.ID, "placement_id", row.ID, "error", err)
	}
}

// placementRow is the record of one object on the far side, before anything
// says who put it there.
func placementRow(kind string, target integrations.Target,
	p integrations.Placement) *database.IntegrationPlacement {

	return &database.IntegrationPlacement{
		ID:             uuid.NewString(),
		ConnectionID:   target.Connection.ID,
		ConnectionName: target.Connection.Name,
		Kind:           kind,
		Destination:    target.Destination,
		ItemID:         p.ItemID,
		ItemURL:        p.ItemURL,
		Name:           p.Name,
		Bytes:          p.Bytes,
		ContentType:    p.Type,
		SHA256:         p.SHA256,
	}
}

// appURL is the link back to the app that rides on an outbound message.
//
// The base here is the *platform's* public address rather than the apps
// origin's — a person following a link out of a notification is going to the
// app as a person, and on a split-origin install the platform's address is the
// one that can sign them in on the way. The path is builder's, because there is
// one definition of it (builder.PublicURL) and this used to be a second.
//
// The app is read for its hostname: an app moved to one of its own is linked
// there, and only the slug is known when the read fails.
func (s *Server) appURL(baseURL, appID, slug string) string {
	app, err := s.db.GetApp(appID)
	if err != nil || app == nil {
		app = &database.App{Slug: slug}
	}
	return builder.PublicURL(baseURL, app)
}

// ------------------------------------------------------- the two adapters

// mailTransport is internal/mail behind the registry's shape.
//
// Mail is the one kind with no credential of its own here: the relay is
// configured on the Email settings page, where an operator looks for it, and a
// second copy on a connection would be a second thing to rotate.
type mailTransport struct{ s *Server }

func (t mailTransport) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	if t.s.mailer == nil {
		return integrations.Result{}, errors.New("email is not available on this install")
	}
	sender, ok := t.s.mailer.(appMailSender)
	if !ok {
		return integrations.Result{}, errors.New("this build's mail service cannot send for an app")
	}
	// Where replies go: the address the app's own people set, which is this
	// kind's destination, or the app owner's own — and for a group-owned app,
	// which has no address, the install's reply-to, which is what an empty
	// value means to the mailer. Never anything app code sent: a Reply-To an
	// app chooses routes answers to an address the company does not control,
	// while wearing a From that it does.
	replyTo := target.Destination.ID
	if replyTo == "" {
		replyTo = target.Destination.Label
	}
	if replyTo == "" {
		replyTo, _ = t.s.appReplyTo(msg.AppID)
	}
	attachments := make([]mailer.Attachment, 0, len(msg.Files))
	for _, f := range msg.Files {
		attachments = append(attachments,
			mailer.Attachment{Name: f.Name, Type: f.Type, Content: f.Content})
	}
	// The install's name for the sender suffix and the frame comes from the
	// setting, here, and never from the job: nothing app code sent can reach it.
	built := mailer.BuildAppMessage(t.s.db.ResolveSenderName(), msg.AppName, msg.AppURL, replyTo,
		msg.Subject, msg.Text, msg.HTML, msg.To, msg.CC, msg.BCC, attachments)
	// Marked retryable wholesale: a relay failure is nearly always transient,
	// the retry is capped at one, and the cost of retrying a permanent failure
	// is one extra log line rather than a duplicate message — the send is
	// idempotent on the transport's side because it either left or it did not.
	if err := sender.SendForApp(ctx, built); err != nil {
		return integrations.Result{}, appint.Retryable(err)
	}
	return integrations.Result{}, nil
}

// appReplyTo is where replies to this app's mail go when its people have not
// chosen: the app owner's own address, or — for a group-owned app, because a
// group has no address — the install's reply-to. The second value says which,
// so the Integrations tab can say so too.
func (s *Server) appReplyTo(appID string) (address, kind string) {
	app, err := s.db.GetApp(appID)
	if err != nil || app == nil {
		return "", ""
	}
	if app.OwnerID != "" {
		owner, err := s.db.GetUser(app.OwnerID)
		if err == nil && owner != nil && owner.Email != "" {
			return owner.Email, replyToOwner
		}
	}
	settings, err := s.db.GetEmailSettings()
	if err != nil {
		return "", ""
	}
	return settings.ReplyTo, replyToInstall
}

// The two things reply_to_fallback_kind can say.
const (
	replyToOwner   = "owner"
	replyToInstall = "install"
)

// recordSendFailure writes the app_events row an owner sees on the tab they
// already read.
//
// Failures only. Forty successful sends a day in that feed would drown the one
// deploy failure it exists to surface — which is exactly what ADR §21 built it
// to prevent. Where an owner goes to see everything is the Integrations tab.
func (s *Server) recordSendFailure(j appint.Job, cause string) {
	where := j.Send.Kind
	if j.Send.Destination != "" {
		where = j.Send.Kind + " (" + j.Send.Destination + ")"
	}
	// Written straight to app_events rather than through events.Recorder, which
	// would also try to *notify* about it. Telling somebody by email that an
	// email did not send is a loop with an obvious failure mode, and the useful
	// detail — which recipients, which channel, which error — is on the app's
	// Integrations tab either way. The record is what a workflow relies on.
	if err := s.db.InsertAppEvent(&database.AppEvent{
		ID:           uuid.NewString(),
		AppID:        j.Send.AppID,
		Kind:         "notify_failed",
		Severity:     database.EventSeverityWarning,
		Source:       database.EventSourceIntegrations,
		Summary:      "a message this app sent did not go out (" + where + ")",
		Detail:       cause,
		DeploymentID: j.Send.DeploymentID,
		// One fingerprint per kind, so a relay that is down all morning is one
		// open problem rather than one per report — and a failure of a
		// *different* integration is still its own.
		Fingerprint: "app_send:" + j.Send.Kind,
		OccurredAt:  time.Now().UTC(),
	}); errors.Is(err, database.ErrAppGone) {
		// The app was deleted while its send was in flight. There is nothing to
		// record it against and nobody to read it — expected, not a fault.
		slog.Debug("app send failure dropped: the app was deleted",
			"send_id", j.Send.ID, "app_id", j.Send.AppID)
	} else if err != nil {
		slog.Error("recording an app send failure failed",
			"send_id", j.Send.ID, "app_id", j.Send.AppID, "error", err)
	}
}

// appLinkBase is where a link back to an app should point. The same resolution
// email and Slack already share, so a notification and an app-initiated message
// cannot name two different installs.
func (s *Server) appLinkBase() string {
	if s.mailer != nil {
		if base := s.mailer.BaseURL(); base != "" {
			return base
		}
	}
	if s.slack != nil {
		if base := s.slack.BaseURL(); base != "" {
			return base
		}
	}
	return s.db.ResolvePublicBaseURL(s.cfg.BaseURL)
}

// appMailSender is the one capability this file needs beyond the Mailer
// interface.
//
// A narrow assertion rather than a method on Mailer so that a test double for
// platform notifications does not have to grow one it will never use — and so
// that an install built without a relay still compiles.
type appMailSender interface {
	SendForApp(ctx context.Context, msg mailer.Message) error
}
