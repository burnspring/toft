package graph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"toft/internal/appint"
	"toft/internal/integrations"
)

// stub stands in for Graph. Every test here points the package's own endpoints
// at it, which is why they are variables in the test binary rather than
// constants: production has no way to move them, and a configurable API base is
// a way to aim a stored credential at an arbitrary server.
func stub(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	oldLogin, oldAPI, oldClient := loginHost, apiBase, httpClient
	loginHost, apiBase = server.URL, server.URL
	httpClient = server.Client()
	tokens.Reset()
	t.Cleanup(func() { loginHost, apiBase, httpClient = oldLogin, oldAPI, oldClient })
	return server
}

func creds() Credentials {
	return Credentials{TenantID: "t", ClientID: "c", ClientSecret: "s"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Below Graph's 4 MiB ceiling a file goes in one PUT, and the request carries
// the one rule this product does not make configurable.
func TestASmallFileGoesInOnePutAndNeverReplaces(t *testing.T) {
	var gotPath, gotQuery string
	var gotBody []byte
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			writeJSON(w, 200, map[string]string{"access_token": "tok"})
			return
		}
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		writeJSON(w, 201, Item{ID: "01ITEM", Name: "june 1.csv", WebURL: "https://example/june1"})
	})

	client, err := Connect(context.Background(), creds())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	item, err := client.Upload(context.Background(), "DRIVE", "FOLDER", "june.csv",
		[]byte("a,b\n1,2\n"), "text/csv")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if !strings.Contains(gotQuery, "conflictBehavior=rename") {
		t.Errorf("the upload did not ask for rename-on-conflict: %q", gotQuery)
	}
	if !strings.Contains(gotPath, "FOLDER") || !strings.Contains(gotPath, "june.csv") {
		t.Errorf("the upload went to %q", gotPath)
	}
	if string(gotBody) != "a,b\n1,2\n" {
		t.Errorf("the body was rewritten: %q", gotBody)
	}
	// The far side renamed it, and the record has to say what is actually there
	// rather than what was asked for.
	if item.Name != "june 1.csv" {
		t.Errorf("item name = %q, want the name Graph gave it", item.Name)
	}
}

// Above the ceiling it goes through an upload session, in ranges, and the item
// only exists when the last range lands. That is what makes a dying session a
// partial *session* rather than a truncated spreadsheet somebody opens.
func TestALargeFileGoesThroughAnUploadSession(t *testing.T) {
	content := make([]byte, SimpleUploadLimit+ChunkSize/2)
	for i := range content {
		content[i] = byte(i)
	}
	var ranges []string
	var created bool

	server := stub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			writeJSON(w, 200, map[string]string{"access_token": "tok"})
		case strings.HasSuffix(r.URL.Path, "/createUploadSession"):
			created = true
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "rename") {
				t.Errorf("the session did not ask for rename-on-conflict: %s", body)
			}
			writeJSON(w, 200, map[string]string{"uploadUrl": apiBase + "/upload/session-1"})
		case strings.Contains(r.URL.Path, "/upload/session-1"):
			rangeHeader := r.Header.Get("Content-Range")
			ranges = append(ranges, rangeHeader)
			// The upload URL carries its own credential; a bearer token beside
			// it is what Graph rejects.
			if r.Header.Get("Authorization") != "" {
				t.Error("a range carried the bearer token as well as the session URL")
			}
			body, _ := io.ReadAll(r.Body)
			total := len(content)
			if strings.HasSuffix(rangeHeader, fmt.Sprintf("/%d", total)) &&
				strings.Contains(rangeHeader, fmt.Sprintf("-%d/", total-1)) {
				writeJSON(w, 201, Item{ID: "01BIG", Name: "big.csv", WebURL: "https://example/big"})
				return
			}
			_ = body
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	_ = server

	client, err := Connect(context.Background(), creds())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	item, err := client.Upload(context.Background(), "DRIVE", "", "big.csv", content, "text/csv")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !created {
		t.Fatal("a file over the ceiling did not create an upload session")
	}
	if len(ranges) < 2 {
		t.Fatalf("the file went in %d range(s), want more than one", len(ranges))
	}
	if item.ID != "01BIG" {
		t.Errorf("item = %+v", item)
	}
}

// A throttle or a server error could succeed on a second attempt; "the site
// does not exist" and "you may not touch it" could not, and retrying them only
// doubles the log noise for a problem a person must fix.
func TestOnlyThrottlesAndServerErrorsAreRetryable(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		retry  bool
	}{
		{http.StatusTooManyRequests, "activityLimitReached", true},
		{http.StatusServiceUnavailable, "serviceNotAvailable", true},
		{http.StatusNotFound, "itemNotFound", false},
		{http.StatusForbidden, "accessDenied", false},
		{http.StatusBadRequest, "invalidRequest", false},
	} {
		stub(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/token") {
				writeJSON(w, 200, map[string]string{"access_token": "tok"})
				return
			}
			writeJSON(w, tc.status, map[string]any{
				"error": map[string]string{"code": tc.code, "message": "Graph's own words"},
			})
		})

		client, err := Connect(context.Background(), creds())
		if err != nil {
			t.Fatalf("Connect: %v", err)
		}
		_, err = client.Upload(context.Background(), "D", "", "x.csv", []byte("x"), "text/csv")
		if err == nil {
			t.Fatalf("%d was treated as success", tc.status)
		}
		if got := appint.IsRetryable(err); got != tc.retry {
			t.Errorf("%s retryable = %v, want %v", tc.code, got, tc.retry)
		}
		if !strings.Contains(err.Error(), "Graph's own words") {
			t.Errorf("%s lost Graph's message: %v", tc.code, err)
		}
	}
}

// A refused token says what Entra said, first line only: the description is a
// paragraph with a correlation id and a stack of URLs, and the first line is
// the part an administrator acts on.
func TestARefusedTokenCarriesEntrasFirstLine(t *testing.T) {
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 401, map[string]string{
			"error":             "invalid_client",
			"error_description": "AADSTS7000215: Invalid client secret provided.\r\nTrace ID: abc\r\nSee https://…",
		})
	})
	_, err := Token(context.Background(), creds())
	if err == nil {
		t.Fatal("a refused token succeeded")
	}
	if !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("the refusal lost Entra's own words: %v", err)
	}
	if strings.Contains(err.Error(), "Trace ID") {
		t.Errorf("the refusal carried the whole paragraph: %v", err)
	}
}

// A refusal without OAuth error fields says its status, never its body.
func TestARefusedTokenWithoutErrorFieldsDoesNotQuoteItsBody(t *testing.T) {
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html>BODYTEXT upstream gateway</html>"))
	})
	_, err := Token(context.Background(), creds())
	if err == nil {
		t.Fatal("a refused token succeeded")
	}
	if strings.Contains(err.Error(), "BODYTEXT") {
		t.Errorf("the refusal quoted the body: %v", err)
	}
}

// The three shapes people actually paste for a SharePoint site.
func TestSiteSpecsPeoplePaste(t *testing.T) {
	for _, tc := range []struct{ in, host, path string }{
		{"contoso.sharepoint.com/sites/finance", "contoso.sharepoint.com", "sites/finance"},
		{"https://contoso.sharepoint.com/sites/finance", "contoso.sharepoint.com", "sites/finance"},
		{"https://contoso.sharepoint.com/sites/finance/", "contoso.sharepoint.com", "sites/finance"},
		{"contoso.sharepoint.com", "contoso.sharepoint.com", ""},
		{"  ", "", ""},
	} {
		host, path := splitSiteSpec(tc.in)
		if host != tc.host || path != tc.path {
			t.Errorf("%q → (%q, %q), want (%q, %q)", tc.in, host, path, tc.host, tc.path)
		}
	}
}

// The folder picker lists folders and not files: a menu of every spreadsheet in
// the library is not a destination picker.
func TestFoldersListsOnlyFolders(t *testing.T) {
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			writeJSON(w, 200, map[string]string{"access_token": "tok"})
			return
		}
		writeJSON(w, 200, map[string]any{"value": []map[string]any{
			{"id": "F1", "name": "Reports", "folder": map[string]int{"childCount": 3}},
			{"id": "I1", "name": "notes.txt"},
			{"id": "F2", "name": "Archive", "folder": map[string]int{"childCount": 0}},
		}})
	})
	client, err := Connect(context.Background(), creds())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	folders, err := client.Folders(context.Background(), "DRIVE")
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 2 || folders[0].Name != "Reports" || folders[1].Name != "Archive" {
		t.Errorf("folders = %+v", folders)
	}
}

// The session URL is the credential: a transfer that fails must not put it in
// the error, which reaches the ledger, the app's events and the log.
func TestAFailedSessionUploadDoesNotRepeatItsURL(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	dead := closed.URL
	closed.Close()
	content := make([]byte, SimpleUploadLimit+1)
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			writeJSON(w, 200, map[string]string{"access_token": "tok"})
		case strings.HasSuffix(r.URL.Path, "/createUploadSession"):
			writeJSON(w, 200, map[string]string{"uploadUrl": dead + "/upload/s1?tempauth=SECRET"})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	client, err := Connect(context.Background(), creds())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err = client.Upload(context.Background(), "DRIVE", "", "big.csv", content, "text/csv")
	if err == nil {
		t.Fatal("an upload to a closed port succeeded")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "tempauth") {
		t.Errorf("the error repeats the session URL: %v", err)
	}
	if !appint.IsRetryable(err) {
		t.Errorf("a transport failure is no longer retryable: %v", err)
	}
}

// The menu is the validator, so it pages: a folder past the first page must
// still be one an editor can choose.
func TestFoldersFollowsTheNextLink(t *testing.T) {
	var queries []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			writeJSON(w, 200, map[string]string{"access_token": "tok"})
			return
		}
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("$skiptoken") == "" {
			writeJSON(w, 200, map[string]any{
				"value": []map[string]any{
					{"id": "F1", "name": "Reports", "folder": map[string]int{"childCount": 3}},
					{"id": "F2", "name": "Archive", "folder": map[string]int{"childCount": 0}},
				},
				"@odata.nextLink": apiBase + "/drives/DRIVE/root/children?$skiptoken=page2",
			})
			return
		}
		writeJSON(w, 200, map[string]any{"value": []map[string]any{
			{"id": "F3", "name": "Zeta", "folder": map[string]int{"childCount": 1}},
		}})
	})
	client, err := Connect(context.Background(), creds())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	folders, err := client.Folders(context.Background(), "DRIVE")
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 3 || folders[2].ID != "F3" || len(queries) != 2 {
		t.Errorf("folders = %+v after %d requests", folders, len(queries))
	}
}

// A next link on another host would send the bearer token there; it is refused,
// and no second request is made.
func TestFoldersRefusesANextLinkOnAnotherHost(t *testing.T) {
	calls := 0
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the next link on another host was followed: %s", r.URL)
	}))
	defer elsewhere.Close()
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			writeJSON(w, 200, map[string]string{"access_token": "tok"})
			return
		}
		calls++
		writeJSON(w, 200, map[string]any{
			"value":           []map[string]any{{"id": "F1", "name": "Reports", "folder": map[string]int{}}},
			"@odata.nextLink": elsewhere.URL + "/v1.0/drives/DRIVE/root/children?$skiptoken=x",
		})
	})
	client, err := Connect(context.Background(), creds())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := client.Folders(context.Background(), "DRIVE"); err == nil {
		t.Error("a next link on another host was accepted")
	}
	if calls != 1 {
		t.Errorf("%d listing requests, want 1", calls)
	}
}

// One grant, two resources: Token asks for Graph and TokenFor for whatever it is
// given — the Bot Connector's scope is the other caller.
func TestTokenForAsksForTheScopeItIsGiven(t *testing.T) {
	var scopes []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		scopes = append(scopes, r.PostForm.Get("scope"))
		writeJSON(w, 200, map[string]string{"access_token": "tok"})
	})
	if _, err := Token(context.Background(), creds()); err != nil {
		t.Fatal(err)
	}
	if _, err := TokenFor(context.Background(), creds(), "https://api.botframework.com/.default"); err != nil {
		t.Fatal(err)
	}
	want := []string{GraphScope, "https://api.botframework.com/.default"}
	if len(scopes) != 2 || scopes[0] != want[0] || scopes[1] != want[1] {
		t.Errorf("scopes asked for = %v, want %v", scopes, want)
	}
}

// The cache answers a second call for the same credential, and a rotated
// secret is a miss: the old secret's token is never handed out for the new.
func TestTheTokenIsCachedPerSecret(t *testing.T) {
	mints := 0
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		mints++
		writeJSON(w, 200, map[string]any{"access_token": fmt.Sprintf("tok%d", mints), "expires_in": 3599})
	})
	first, err := Mint(context.Background(), creds(), GraphScope)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Mint(context.Background(), creds(), GraphScope)
	if again.Value != first.Value || mints != 1 {
		t.Errorf("second mint = %q after %d requests, want the cached %q", again.Value, mints, first.Value)
	}
	if left := time.Until(first.ExpiresAt); left < 59*time.Minute || left > time.Hour {
		t.Errorf("expiry is %v away, want about an hour", left)
	}
	rotated := creds()
	rotated.ClientSecret = "rotated"
	if tok, _ := Mint(context.Background(), rotated, GraphScope); tok.Value == first.Value || mints != 2 {
		t.Errorf("a rotated secret got %q after %d requests", tok.Value, mints)
	}
}

// jwt builds a token whose payload carries these roles, which is all
// CheckFileRoles reads.
func jwt(roles ...string) string {
	payload, _ := json.Marshal(map[string]any{"aud": Resource, "roles": roles})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// A token carrying any permission beyond files is never handed to an app. A
// failure here hands an app the tenant's mail, and nothing on the screen
// changes.
func TestATokenWithANonFileRoleIsRefused(t *testing.T) {
	for _, roles := range [][]string{
		{"Mail.Send"},
		{"Sites.Read.All"},
		{"Files.ReadWrite.All"},
		{"Sites.Selected", "Mail.Send"},
	} {
		err := CheckFileRoles(jwt(roles...))
		var wide *integrations.TokenTooWideError
		if !errors.As(err, &wide) {
			t.Errorf("%v: err = %v, want a too-wide refusal", roles, err)
			continue
		}
		if slices.Contains(wide.Roles, "Sites.Selected") || len(wide.Roles) == 0 {
			t.Errorf("%v: the refusal names %v", roles, wide.Roles)
		}
	}
	for _, roles := range [][]string{
		{"Sites.Selected"},
		{"Files.SelectedOperations.Selected"},
		{"ListItems.SelectedOperations.Selected", "Lists.SelectedOperations.Selected"},
		nil,
	} {
		if err := CheckFileRoles(jwt(roles...)); err != nil {
			t.Errorf("%v was refused: %v", roles, err)
		}
	}
	if err := CheckFileRoles("not-a-jwt"); err == nil {
		t.Error("a token whose roles cannot be read was let through")
	}
}
