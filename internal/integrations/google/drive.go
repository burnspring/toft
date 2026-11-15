package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"strings"

	"bytes"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations"
)

// Drive writes an app's files into a Google Drive folder.
//
// The scope is one folder — a shared drive's root, or a folder inside it — and
// it is welded like every other scope here. The service account is a member of
// the shared drive from Google's side, which is the boundary and the one an
// administrator can change without touching Toft. **A shared drive, not a My
// Drive folder**: a service account owns no storage, so a file it creates in a
// person's My Drive fails with `storageQuotaExceeded`, while a shared drive owns
// its files itself.
//
// **Drive adds and never replaces, by construction rather than by a flag.**
// `files.create` always creates: a second file with the same name is a second
// file, and there is no path through this transport that reaches `files.update`.
// That is the same rule the Microsoft trio gets from `conflictBehavior: rename`,
// arrived at by the API's own shape.
type Drive struct{}

var (
	_ integrations.Transport  = Drive{}
	_ integrations.Enumerator = Drive{}
	_ integrations.Verifier   = Drive{}
	_ integrations.Issuer     = Drive{}
)

type driveFile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	WebLink string `json:"webViewLink"`
	Size    string `json:"size"`
}

// Send uploads one file and reports the placement.
func (Drive) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	if msg.File == nil {
		return integrations.Result{}, errors.New("this integration takes a file")
	}
	if err := appint.CheckFilename(msg.File.Name); err != nil {
		return integrations.Result{}, err
	}
	token, err := token(ctx, target, ScopeDrive)
	if err != nil {
		return integrations.Result{}, err
	}

	// The app's people choose a folder inside the administrator's scope; with
	// none chosen the file lands in the scope itself.
	parent := target.Destination.ID
	if parent == "" {
		parent = strings.TrimSpace(target.Connection.ScopeID)
	}

	body, contentType, err := multipartUpload(msg.File.Name, parent,
		msg.File.Type, msg.File.Content)
	if err != nil {
		return integrations.Result{}, err
	}

	var file driveFile
	endpoint := driveHost + "/upload/drive/v3/files?uploadType=multipart" +
		"&supportsAllDrives=true&fields=id,name,webViewLink,size"
	if err := call(ctx, token, "POST", endpoint, contentType, body, &file,
		"the upload"); err != nil {
		return integrations.Result{}, err
	}

	return integrations.Result{
		Placement: &integrations.Placement{
			ItemID:  file.ID,
			ItemURL: file.WebLink,
			Name:    file.Name,
			Bytes:   int64(len(msg.File.Content)),
			Type:    msg.File.Type,
			SHA256:  appint.DigestBytes(msg.File.Content),
		},
	}, nil
}

// Destinations lists the folders directly inside the connection's scope.
//
// One level, for the reason the Microsoft folder picker is one level: a
// recursive tree of an organization's drive is a lot of requests and a menu
// nobody can read, and a nested folder is a case for a second connection scoped
// to it.
func (Drive) Destinations(ctx context.Context,
	target integrations.Target) ([]database.Destination, error) {

	token, err := token(ctx, target, ScopeDrive)
	if err != nil {
		return nil, err
	}
	parent := strings.TrimSpace(target.Connection.ScopeID)
	if parent == "" {
		return nil, errors.New("this connection has no folder")
	}
	query := url.Values{
		"q": {fmt.Sprintf(
			"'%s' in parents and mimeType = 'application/vnd.google-apps.folder' and trashed = false",
			strings.ReplaceAll(parent, "'", `\'`))},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
		// nextPageToken has to be in the field mask: without it Drive does not
		// return the token at all, and the menu — which is also the validator —
		// silently stops at the first page.
		"fields":   {"nextPageToken,files(id,name)"},
		"pageSize": {"200"},
	}
	var files []driveFile
	for page := 0; page < listPageCap; page++ {
		var list struct {
			Files         []driveFile `json:"files"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := call(ctx, token, "GET",
			driveHost+"/drive/v3/files?"+query.Encode(), "", nil, &list,
			"the folder list"); err != nil {
			return nil, err
		}
		files = append(files, list.Files...)
		if list.NextPageToken == "" {
			break
		}
		query.Set("pageToken", list.NextPageToken)
	}
	out := make([]database.Destination, 0, len(files))
	for _, f := range files {
		out = append(out, database.Destination{ID: f.ID, Label: f.Name})
	}
	return out, nil
}

// listPageCap bounds the paging of a destination menu, as slack's
// channelPageCap does: an unbounded loop against someone else's API is how a
// settings page hangs.
const listPageCap = 10

// Verify proves the credential *and* that the folder was shared with the
// service account, which is the half an administrator most often has not done.
//
// The refusal names the address to share with, because "permission denied" on
// its own sends somebody to Google's console with nothing to look for.
func (Drive) Verify(ctx context.Context, target integrations.Target) (string, error) {
	sa, err := From(target)
	if err != nil {
		return "", err
	}
	folder := strings.TrimSpace(target.Connection.ScopeID)
	if folder == "" {
		return "", errors.New(
			"a Google Drive connection needs the id of the folder it writes into — " +
				"it is the last part of the folder's URL")
	}
	accessToken, err := sa.Token(ctx, ScopeDrive)
	if err != nil {
		return "", err
	}
	var file driveFile
	endpoint := driveHost + "/drive/v3/files/" + url.PathEscape(folder) +
		"?supportsAllDrives=true&fields=id,name"
	if err := call(ctx, accessToken, "GET", endpoint, "", nil, &file, "that folder"); err != nil {
		return "", fmt.Errorf("%w — add %s to the shared drive that holds the folder, as "+
			"Contributor or above", err, sa.ClientEmail)
	}
	if file.Name == "" {
		return folder, nil
	}
	return file.Name, nil
}

// Token hands an app the service account's token for the drive scope — the
// one the transport uses, and no other. A service-account token has no roles
// to read: its reach is what has been shared with the account's address, which
// the kind's hint already says to keep to this connection alone.
func (Drive) Token(ctx context.Context, target integrations.Target) (integrations.IssuedToken, error) {
	sa, err := From(target)
	if err != nil {
		return integrations.IssuedToken{}, err
	}
	tok, err := sa.Mint(ctx, ScopeDrive)
	if err != nil {
		return integrations.IssuedToken{}, err
	}
	folder := integrations.TokenFolder{ItemID: target.Destination.ID, Label: target.Destination.Label}
	if folder.ItemID == "" {
		folder = integrations.TokenFolder{
			ItemID: strings.TrimSpace(target.Connection.ScopeID), Label: target.Connection.ScopeLabel,
		}
	}
	return integrations.IssuedToken{AccessToken: tok, Resource: Resource, Folder: folder}, nil
}

// CheckToken has nothing to refuse: a service-account token carries no roles.
func (Drive) CheckToken(context.Context, integrations.Target) error { return nil }

// Resource is what a Drive token is for, as an app's SDK names it.
const Resource = "https://www.googleapis.com"

// multipartUpload builds Drive's metadata-plus-content body.
func multipartUpload(name, parent, contentType string, content []byte) ([]byte, string, error) {
	metadata := map[string]any{"name": name}
	if parent != "" {
		metadata["parents"] = []string{parent}
	}
	meta, err := jsonBytes(metadata)
	if err != nil {
		return nil, "", err
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	metaPart, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"application/json; charset=UTF-8"},
	})
	if err != nil {
		return nil, "", err
	}
	if _, err := metaPart.Write(meta); err != nil {
		return nil, "", err
	}

	if contentType == "" {
		contentType = "application/octet-stream"
	}
	filePart, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {contentType}})
	if err != nil {
		return nil, "", err
	}
	if _, err := filePart.Write(content); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "multipart/related; boundary=" + w.Boundary(), nil
}

// jsonBytes is json.Marshal, named so the multipart builder reads as prose.
func jsonBytes(v any) ([]byte, error) { return json.Marshal(v) }

// token mints one for a scope, from the Target's credential.
func token(ctx context.Context, target integrations.Target, scope string) (string, error) {
	sa, err := From(target)
	if err != nil {
		return "", err
	}
	return sa.Token(ctx, scope)
}
