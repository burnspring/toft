// Package msfiles is the SharePoint and OneDrive transports, which are one
// mechanism twice.
//
// They differ in exactly one thing — which drive a file lands in — so they are
// one file with a flag rather than two packages that would have to be kept in
// step. SharePoint writes into a site's default document library; OneDrive
// writes into one person's drive. Everything after "which drive": the folder
// picker, the never-replace rule, the upload-session threshold and the
// placement record, is identical, and the day it stops being identical is the
// day this splits.
//
// # What these do not do
//
// **Nothing here reads on an app's behalf.** Every action Toft performs is
// outbound. What an app may do instead, when its connection allows it, is hold
// the connection's own token (Token) and read with Microsoft's SDK — the grant
// on the far side is then the only fence, and the switch says so. An app that could list a
// document library or fetch a file back is a data-access product with its own
// permission model, its own audit questions and its own quota, and this
// platform already has that surface — app storage, where a grant is a person's
// and a mapping is an app's. A read verb here would put a second, weaker answer
// beside it.
//
// **No list items.** Appending a row to a SharePoint list means the platform
// learning that list's columns, and a type mismatch is a 400 an app author
// cannot debug from outside. A file has one shape.
package msfiles

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations"
	"toft/internal/integrations/graph"
)

// Transport writes files into a Microsoft drive.
//
// `personal` picks which drive the connection's scope names: a SharePoint site,
// or one person's OneDrive.
type Transport struct{ personal bool }

var (
	_ integrations.Transport  = Transport{}
	_ integrations.Enumerator = Transport{}
	_ integrations.Verifier   = Transport{}
	_ integrations.Issuer     = Transport{}
)

// SharePoint writes into a site's default document library.
func SharePoint() Transport { return Transport{} }

// OneDrive writes into one person's drive.
func OneDrive() Transport { return Transport{personal: true} }

// Send uploads one file and reports the placement.
//
// **The Result always carries a Placement, and that is not optional.** These
// are the kinds whose descriptor says they leave residue, and the placement is
// the only durable record that this app put this object into that library —
// the row that survives the ledger's retention window and the app's own
// deletion. A send here that returned no placement would be a file in somebody's
// document store that nothing on this platform remembers.
func (t Transport) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	if msg.File == nil {
		return integrations.Result{}, errors.New("this integration takes a file")
	}
	if err := appint.CheckFilename(msg.File.Name); err != nil {
		return integrations.Result{}, err
	}

	client, driveID, err := t.drive(ctx, target)
	if err != nil {
		return integrations.Result{}, err
	}

	// The folder is the attachment's and the name is the app's. An empty folder
	// is the library's root, which is what "use the default" means here.
	item, err := client.Upload(ctx, driveID, target.Destination.ID,
		msg.File.Name, msg.File.Content, msg.File.Type)
	if err != nil {
		return integrations.Result{}, err
	}

	return integrations.Result{
		Placement: &integrations.Placement{
			ItemID:  item.ID,
			ItemURL: item.WebURL,
			// The name the far side gave it, which is not always the name the
			// app asked for: a collision renames, and the record has to say
			// what is actually there rather than what was requested.
			Name:   item.Name,
			Bytes:  int64(len(msg.File.Content)),
			Type:   msg.File.Type,
			SHA256: appint.DigestBytes(msg.File.Content),
		},
	}, nil
}

// Destinations lists the folders at the top of the drive.
func (t Transport) Destinations(ctx context.Context,
	target integrations.Target) ([]database.Destination, error) {

	client, driveID, err := t.drive(ctx, target)
	if err != nil {
		return nil, err
	}
	folders, err := client.Folders(ctx, driveID)
	if err != nil {
		return nil, err
	}
	out := make([]database.Destination, 0, len(folders))
	for _, f := range folders {
		out = append(out, database.Destination{ID: f.ID, Label: f.Name})
	}
	return out, nil
}

// Verify proves the credential *and* the scope at save time, and returns the
// scope's own label.
//
// Two things at once, deliberately. A tenant credential that mints a token
// proves nothing about whether `Sites.Selected` was granted for this site, and
// the grant is the half an administrator most often has not done yet — so the
// check is a read of the site, which is the same call the first upload would
// have made six weeks later inside a queue worker.
func (t Transport) Verify(ctx context.Context, target integrations.Target) (string, error) {
	scope := strings.TrimSpace(target.Connection.ScopeID)
	if scope == "" {
		if t.personal {
			return "", errors.New(
				"a OneDrive connection needs the sign-in name of the account whose drive it writes to")
		}
		return "", errors.New(
			"a SharePoint connection needs the site, like contoso.sharepoint.com/sites/finance")
	}
	client, err := graph.Connect(ctx, graph.From(target))
	if err != nil {
		return "", err
	}
	if t.personal {
		_, label, err := client.UserDrive(ctx, scope)
		return label, err
	}
	site, err := client.ResolveSite(ctx, scope)
	if err != nil {
		return "", fmt.Errorf("%w — if the credential is right, the tenant administrator "+
			"has not granted this application access to that site yet", err)
	}
	label := site.DisplayName
	if label == "" {
		label = site.Name
	}
	return label, nil
}

// Token hands an app the connection's Graph token, after checking that every
// permission it carries is a file one (graph.CheckFileRoles). Checked on every
// call, not when the switch was thrown: a tenant administrator can add a
// permission to the Entra app at any time and Toft would never hear of it.
//
// The folder is the attachment's, as Graph ids, so the app's code names no site.
func (t Transport) Token(ctx context.Context, target integrations.Target) (integrations.IssuedToken, error) {
	tok, err := graph.Mint(ctx, graph.From(target), graph.GraphScope)
	if err != nil {
		return integrations.IssuedToken{}, err
	}
	if err := graph.CheckFileRoles(tok.Value); err != nil {
		return integrations.IssuedToken{}, err
	}
	_, driveID, err := t.drive(ctx, target)
	if err != nil {
		return integrations.IssuedToken{}, err
	}
	item := target.Destination.ID
	if item == "" {
		item = "root"
	}
	return integrations.IssuedToken{
		AccessToken: tok,
		Resource:    graph.Resource,
		Folder: integrations.TokenFolder{
			DriveID: driveID, ItemID: item, Label: target.Destination.Label,
		},
	}, nil
}

// CheckToken mints (or reads the cached token) and checks its roles, without
// the two Graph reads that resolve the folder.
func (t Transport) CheckToken(ctx context.Context, target integrations.Target) error {
	tok, err := graph.Mint(ctx, graph.From(target), graph.GraphScope)
	if err != nil {
		return err
	}
	return graph.CheckFileRoles(tok.Value)
}

// drive resolves the connection's scope to a drive id.
func (t Transport) drive(ctx context.Context, target integrations.Target) (*graph.Client, string, error) {
	client, err := graph.Connect(ctx, graph.From(target))
	if err != nil {
		return nil, "", err
	}
	scope := strings.TrimSpace(target.Connection.ScopeID)
	if t.personal {
		id, _, err := client.UserDrive(ctx, scope)
		return client, id, err
	}
	site, err := client.ResolveSite(ctx, scope)
	if err != nil {
		return nil, "", err
	}
	id, err := client.SiteDrive(ctx, site.ID)
	return client, id, err
}
