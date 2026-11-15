package slack

import (
	"context"
	"errors"

	"toft/internal/database"
	"toft/internal/integrations"
)

// Transport is Slack behind the catalog's registry.
//
// It holds no state at all — no database handle, no encryptor, no settings —
// because everything one send needs arrives on the Target: the connection, the
// credential already decrypted, and the destination already resolved through
// the connection's default. That is what makes a transport a leaf: it can be
// exercised against a recorded fixture without a platform underneath it.
//
// The Service above it keeps the platform-notification half, which is a
// different job — it renders the platform's own templates and answers to the
// audience switches — and the two do not share a code path, which is right,
// because "the platform is telling you something" and "an app wrote this" are
// two things this product goes to some trouble to keep apart.
type Transport struct{}

var (
	_ integrations.Transport  = Transport{}
	_ integrations.Enumerator = Transport{}
)

// Send posts one app-written message, or uploads one file.
func (Transport) Send(ctx context.Context, t integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	cfg, err := configFor(t)
	if err != nil {
		return integrations.Result{}, err
	}
	// A chosen channel that has stopped working fails; it never falls back to
	// the default. Falling back is what an *unset* channel means, and silently
	// rerouting an app's report into a channel its owners did not choose is a
	// disclosure.
	channel := t.Destination.ID
	if channel == "" {
		return integrations.Result{}, errors.New("this app has no Slack channel to post to")
	}

	if msg.Action == "file" {
		if msg.File == nil {
			return integrations.Result{}, errors.New("no file to upload")
		}
		return integrations.Result{}, UploadForApp(ctx, cfg, channel,
			msg.File.Name, msg.File.Content, msg.Title, msg.Comment, msg.AppName, msg.AppURL, msg.Platform)
	}
	return integrations.Result{}, PostForApp(ctx, cfg, channel, msg.Text, msg.AppName, msg.AppURL, msg.Platform)
}

// Destinations lists the conversations this connection's bot can post to.
//
// The bot's membership is the boundary, and this is where that is true rather
// than merely said: an app may post to exactly the channels somebody invited
// the bot into, because those are the only ones this list can contain.
func (Transport) Destinations(ctx context.Context,
	t integrations.Target) ([]database.Destination, error) {

	cfg, err := configFor(t)
	if err != nil {
		return nil, err
	}
	channels, err := Channels(ctx, cfg)
	if err != nil {
		return nil, err
	}
	out := make([]database.Destination, 0, len(channels))
	for _, c := range channels {
		out = append(out, database.Destination{ID: c.ID, Label: c.Name})
	}
	return out, nil
}

// configFor builds the plaintext Config one call needs, from a Target.
func configFor(t integrations.Target) (Config, error) {
	cfg := Config{Token: t.Credential("bot_token"), DefaultChannel: t.Connection.DefaultDestination.ID}
	if err := cfg.validateToken(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
