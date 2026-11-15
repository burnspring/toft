package api

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// Durable storage links (ADR §12a, *A store, in a browser*).
//
// **A storage link is a name at the platform's address and carries no
// authority.** `https://toft.example/files/reports/2026-11/summary.pdf` names a
// store and an object in it; the credential — a presigned GET — is minted only
// at the moment a signed-in reader opens it, through the same resolver every
// person-facing store route uses, and audited as the same `store.presign` row
// with `via: link`. So a link that leaks from an inbox grants nothing, revoking
// a grant refuses the next open with nothing to invalidate, and there is no
// table of links to keep in step with the grants: two authorities for one file
// is what this design refuses.
//
// **Store grants decide, and nothing else.** Not the sending app's audience (a
// batch app has none, and an app's viewers reaching its storage through a link
// would be a grant nobody could see on the Storage page), and no administrator
// bypass. A reader without a grant and a link to a file that is not there get
// the same card, byte for byte: 404 over 403, as the gateway's card keeps.

// filesRoutePrefix is where a storage link lives on the platform origin — a
// prefix of its own, because /storage/{slug}/* is the store browser's folder
// path and one name can be a file in one store and a folder in another.
const filesRoutePrefix = "/files/"

// storageLinkFor is the address of one object: the platform's address, the
// store, and the object name with each segment escaped. The server owns the
// encoding once, rather than four SDKs agreeing on it.
func storageLinkFor(base, slug, name string) string {
	segs := strings.Split(name, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.TrimSuffix(base, "/") + filesRoutePrefix + url.PathEscape(slug) + "/" + strings.Join(segs, "/")
}

// handleOpenStorageLink — GET /files/{slug}/*. Sign-in redirect, reach check,
// existence check, audited mint, 302; or the not-found card.
func (s *Server) handleOpenStorageLink(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() || p.ID == "" {
		base := s.platformBaseFor(r)
		back := base + r.URL.EscapedPath()
		http.Redirect(w, r, base+"/login?redirect="+url.QueryEscape(back), http.StatusFound)
		return
	}
	name, err := url.PathUnescape(chi.URLParam(r, "*"))
	if r.URL.RawPath == "" {
		name, err = chi.URLParam(r, "*"), nil
	}

	// Every refusal below is the one card. The audit row says which of them it
	// was; the reader is told none of it.
	refuse := func(scopes *database.StoreScopes, reason string) {
		s.auditStorageLinkRefused(r, slug, scopes, name, reason)
		s.serveFileNotFound(w)
	}
	if !s.storeUploadsOffered() {
		s.serveFileNotFound(w)
		return
	}
	if err != nil || ValidateStoreObjectName(name) != nil {
		refuse(nil, "invalid_name")
		return
	}
	scopes, err := s.db.StoreScopesBySlug(p.ID, slug)
	if err != nil {
		log.Printf("resolving store reach for a link: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if scopes == nil || !s.dataPlaneOffered(scopes.Resource) {
		refuse(nil, "no_reach")
		return
	}
	level := scopes.LevelForKey(name)
	if _, allowed := presignFor(level, verbGet); !allowed {
		refuse(scopes, "outside_grant")
		return
	}

	key := storeKeyFor(scopes.Resource, name)
	_, scope := scopes.ScopeForKey(name)
	signer := s.signerFor(r, scopes.Resource, scope, 0)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	exists, err := signer.HasObject(ctx, scopes.Resource.Bucket, key)
	if err != nil {
		log.Printf("checking %s on %s for a link: %v", name, scopes.Resource.Slug, err)
		http.Error(w, "the object store did not answer", http.StatusBadGateway)
		return
	}
	if !exists {
		refuse(scopes, "missing")
		return
	}
	signed, err := presignObject(ctx, signer, scopes.Resource.Bucket, key, name, verbGet, false, storeLinkOpenTTL)
	if err != nil {
		log.Printf("presigning a link to %s on %s: %v", name, scopes.Resource.Slug, err)
		http.Error(w, "the object store could not be addressed", http.StatusBadGateway)
		return
	}
	s.auditStorePresignVia(r, scopes, key, name, level, verbGet, "link")
	// The redirect carries a credential: nothing between here and the browser
	// keeps it.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, signed.URL, http.StatusFound)
}

// storeLinkOpenTTL is how long the address a link open redirects to lives.
//
// A browser follows the 302 at once, and both S3 and the host store check
// expiry when a request starts, so a long download still finishes. The hour
// storeURLTTL gives `toft store` would let an address already opened outlive a
// revoked grant by that hour (BB-67): a minute is enough to start, and short
// enough that "revoking refuses the next open" is nearly the whole truth.
const storeLinkOpenTTL = 1 * time.Minute

// auditStorageLinkRefused records an open that was refused, so an access
// review asking "who tried" sees more than who succeeded. The reason is the
// auditor's and never the reader's: the response is the same card for all.
// The target is the store only when the reader reaches some of it; otherwise
// the row carries the slug and the name as asked, and the name is still what
// Activity's search finds it by.
func (s *Server) auditStorageLinkRefused(r *http.Request, slug string, scopes *database.StoreScopes, name, reason string) {
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())
	key, target := name, ""
	if scopes != nil {
		key, target = storeKeyFor(scopes.Resource, name), scopes.Resource.ID
	}
	entry := database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		Action:     "store.link.refused",
		TargetType: "storage_resource", TargetID: target,
		After: jsonString(map[string]string{
			"store":  slug,
			"key":    key,
			"reason": reason,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}
	if err := s.db.WriteAuditRow(entry); err != nil {
		log.Printf("auditing a refused storage link: %v", err)
	}
}

// fileNotFoundTmpl is the card a storage link that opens nothing draws. The
// gateway card's frame and wording rule — it names neither the store nor the
// file, so an unreadable file and a missing one are the same page — with the
// one next step that leaks nothing.
var fileNotFoundTmpl = template.Must(template.New("files404").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Not found</title>
  <style>` + gatewayFrameCSS + `
  </style>
</head>
<body>
` + gatewayFrameHeader + `  <main>
    <div class="content">
      <div class="inner">
        <h1>Not found</h1>
        <p class="lede">There is no file at this address for you — either nothing has this
           name any more, or this account cannot open it. If somebody sent you the link,
           ask them whether you have access to the storage it is in.</p>
{{if .PlatformLink}}        <div class="foot"><a href="{{.PlatformLink}}">Go to Toft →</a></div>
{{end}}      </div>
    </div>
  </main>
</body>
</html>`))

// serveFileNotFound is the one refusal a storage link gives, to a browser and
// to anything else alike.
func (s *Server) serveFileNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	gatewayPageHeaders(w, withFrame(fileNotFoundCardCSP, ""))
	w.WriteHeader(http.StatusNotFound)
	if err := fileNotFoundTmpl.Execute(w, struct{ PlatformLink string }{
		strings.TrimRight(s.platformURL(), "/"),
	}); err != nil {
		log.Printf("storage link 404 render: %v", err)
	}
}

// --- The app's half: building a link ---

// handleAppStoreLink — GET /api/v1/app/store/{slug}/link/*. The address of one
// object in a store mapped to this app, for a message somebody reads later.
//
// It checks what the app can know — the mapping and the name — and nothing
// else. Not that the object exists (a job commonly builds the link before or
// while writing the file, and the open checks anyway), and not who may open
// it, which is the store's grants and not the app's business. Any mapping
// level builds one: the reader's grant decides, so refusing a write-only
// mapping would add a rule the open already makes moot. A volume builds one
// too when it is on a host root — the host data plane is how a person opens
// its files — and is refused like any unmapped name where nobody could.
func (s *Server) handleAppStoreLink(w http.ResponseWriter, r *http.Request) {
	m, ok := s.resolveAppStoreFor(w, r, true, s.dataPlaneOffered)
	if !ok {
		return
	}
	name, ok := storeWildcard(w, r)
	if !ok {
		return
	}
	if err := ValidateStoreObjectName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send a name relative to the store, like "reports/2026/q1.csv", with no leading or trailing slash`})
		return
	}
	base := s.db.ResolvePublicBaseURL(s.cfg.BaseURL)
	if base == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":     "this install has no address of its own, so no link to a file can be built",
			"code":      "no_public_address",
			"retryable": false,
			"hint":      "an administrator sets the Address under Admin → Settings (or BASE_URL in the install's configuration)",
		})
		return
	}
	within := m.within(name)
	link := storageLinkFor(base, m.Resource.Slug, within)
	s.auditAppStoreLink(r, m, storeKeyFor(m.Resource, within))
	writeJSON(w, http.StatusOK, map[string]string{"link": link, "name": name})
}

// auditAppStoreLink records the building of a link — the sender's half of
// "who was sent this", found by searching the file's name beside the opens.
func (s *Server) auditAppStoreLink(r *http.Request, m *appStoreMapping, key string) {
	token := appTokenFromContext(r.Context())
	workload := ""
	if token != nil {
		workload = token.Token.Workload
	}
	meta := requestMeta(r.Context())
	entry := database.AuditEntry{
		ActorType: database.PrincipalTypeApp, ActorID: m.Mapping.AppID,
		Action:     "store.link",
		TargetType: "storage_resource", TargetID: m.Resource.ID,
		After: jsonString(map[string]string{
			"key":      key,
			"folder":   m.Mapping.Folder,
			"workload": workload,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}
	if err := s.db.WriteAuditRow(entry); err != nil {
		log.Printf("audit: store.link for app %s on %s: %v", m.Mapping.AppID, m.Resource.Slug, err)
	}
}
