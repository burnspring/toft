package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations"
)

// A throwaway RSA key, generated for this test file and used nowhere else. It
// exists so the assertion path is exercised end to end rather than stubbed
// around: a JWT that is never signed is a JWT whose signing was never tested.
const testKey = `-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQD41BnK4238QgP4
GMFFFKC/y+DkCmPwmCX3BdYe/2IcZ/qVl/T90P6UEQDj8jJOaa6+Nv1BkZwNiqGJ
AVGefkS4V0/Au3e+Ui7R/N/PYGq/Rbb+egtSM2HyGsl5V84DlyH6yGRWMI9pvKCE
gFGRTlxI8fO5KlFSIWDynojqndiHjBYksOjralp+ipWl1C2RCwO9YhNU57bZ7I8D
++FFsjKfb22ahR2wZpH7MwscWFlCyTDgDOIiynxsYe2kLfdttfAXQhuH8c3EjwQp
yXQ547ny3N9EiHTD5/YmfzXMWmmKFkPr2nRTerDg3UospFCvs0spWsaS577y4gt3
XKQI/hu9AgMBAAECggEAO4l8dmg+i8o76uP5co8B+4JL2CSITywIgXebCSWwEv3q
HlsSmpykHUeIQAJWeR6eWhMMO+EcRr0x83cmUVcDGFyDz6jLePVvEH2CC7uVLxj+
NRt8k/mJaQVxyZ/xwjgqQDgskdbiqbkhazLM0Yi/WhDScS6n8I/eA49dUU3dXmj0
pfTTHxLXSmXo4dGYMyR7Vlm106zCe+UVItsq7BenTf+UrfZy3UEYWLiuzSk3EK8/
Q4Qk5T7mZiKqGG/RvYhWmjzashXOWwRlnpE/QrM8JaGAa2R5r5KNSNBXrlMWf6wt
xZsYcp+394YgrFuN9pIiywRDPM7LcQ3czvmSWMlqvQKBgQD/Kxt7f0Q3M/27Lu3N
gCxGk+awip/mHI+MdFe56+l1w20MvE65QJMNJRr2M5hzqxNzb8dBHuOXC/rxBju0
j/WTqgWDx8i8U+TVt2Za23Q3dXesYH+cVIPOw+WJX06GdgH6mssyvH4fWXWlu08i
mdzYtJY8nUFKkFca4qUjW6Q0LwKBgQD5o7QzIDQS6bAURwaZ+gkxlPOvf1IU3yGq
lYNw6Lyu25pfu5FUAHywrFlskDMq3QGrslWnZGOQMIjwphPr5QPVA4FfhtEuSkVx
WWlKDWdyisFa+Lh9d/2qLdN5WVQ6aEqYEwZf0BcnLkQhlgMr/lwpyfQGjfdsy8nH
YMd65F830wKBgQCPtIpUwfb8E/bI7YQV4SKOWQtpz5h/8wPROVCSr88aeIY4zvCT
Gk/27jfCAorQUb+xmnkgLrJERgX8qeMELLNEh1N1xRVDIwhUaGZnswI/nZNZbDtS
IrLcxqzF8fv6dAlnRrg7ibaKF0rPnK7TsM2U6hdWacX0pVe5F32jbSAMFwKBgCBc
yI+YLSgGg30deA2eDHMRDEZECzAJfSPQHioVAeDpNyoR9tUc+w0sSqt4RinYXM8V
X2VD7mRS5m3MGekK12RV//+SSqvllPsMzEl9U1vWARr6A3DAjXHyXufsXK71tjXg
wYLKRzYdGVgBSof5gZkpRSlUXi8MfCwMr8+Oq4lhAoGBAIWz8tuM+sywDhDu3gH8
mfChBZJZDLqxVL7Y3I1O0G5oUV4FL4Mn7sXzspdwU/MyP/0iTR7d1gt16ogqat5A
vQiwpYrPdUvdNa8lriXuVKREJh2Hc/WvtIOm5DTHfVDA1Qb4QATRoiKd/1hB7hjQ
1lbUAf7ClleK69Zj8z0I7Tcr
-----END PRIVATE KEY-----`

func stub(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(h)
	oldToken, oldDrive, oldChat, oldClient := tokenHost, driveHost, chatHost, httpClient
	tokenHost, driveHost, chatHost = server.URL+"/token", server.URL, server.URL
	httpClient = server.Client()
	tokens.Reset()
	t.Cleanup(func() {
		tokenHost, driveHost, chatHost, httpClient = oldToken, oldDrive, oldChat, oldClient
		server.Close()
	})
}

func target(scope, dest string) integrations.Target {
	key, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "toft@example.iam.gserviceaccount.com",
		"private_key":  testKey,
	})
	return integrations.Target{
		Connection: database.IntegrationConnection{
			ID: "iconn-1", Kind: "gdrive", Name: "Finance", ScopeID: scope,
		},
		Credentials: map[string]string{"service_account": string(key)},
		Destination: database.Destination{ID: dest},
	}
}

func tokenHandler(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"tok"}`))
}

// A key that is not a service account key says so, rather than failing later
// with "invalid signature" and no way to see why.
func TestAKeyThatIsNotOneSaysSo(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"type":"authorized_user"}`} {
		_, err := From(integrations.Target{Credentials: map[string]string{"service_account": raw}})
		if err == nil {
			t.Errorf("%q was accepted as a service account key", raw)
		}
	}
}

// Drive uploads as multipart-related: metadata first, then the bytes. The
// metadata names the parent folder, which is the administrator's, and never
// anything app code sent.
func TestDriveUploadsMetadataThenBytes(t *testing.T) {
	var meta map[string]any
	var content []byte
	var query string

	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			tokenHandler(w)
			return
		}
		query = r.URL.RawQuery
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("content type: %v", err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		part, _ := reader.NextPart()
		body, _ := io.ReadAll(part)
		_ = json.Unmarshal(body, &meta)
		part, _ = reader.NextPart()
		content, _ = io.ReadAll(part)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"F1","name":"june.csv","webViewLink":"https://drive/june"}`))
	})

	result, err := Drive{}.Send(context.Background(), target("ROOT", "FOLDER"),
		integrations.Send{
			Action: "upload",
			File:   &appint.File{Name: "june.csv", Content: []byte("a,b\n"), Type: "text/csv"},
		})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if meta["name"] != "june.csv" {
		t.Errorf("metadata = %+v", meta)
	}
	parents, _ := meta["parents"].([]any)
	if len(parents) != 1 || parents[0] != "FOLDER" {
		t.Errorf("the file did not go to the chosen folder: %+v", meta)
	}
	if string(content) != "a,b\n" {
		t.Errorf("the bytes were rewritten: %q", content)
	}
	if !strings.Contains(query, "uploadType=multipart") {
		t.Errorf("query = %q", query)
	}
	// **The placement is not optional.** This kind leaves residue, and a send
	// that returned none would be a file in somebody's drive that nothing on
	// this platform remembers.
	if result.Placement == nil || result.Placement.ItemID != "F1" {
		t.Fatalf("no placement was recorded: %+v", result)
	}
	if result.Placement.ItemURL == "" {
		t.Error("the placement has no URL — the list exists so a person can go and look")
	}
}

// With no folder chosen the file lands in the connection's scope, which is what
// "use the default" means here.
func TestNoChosenFolderMeansTheConnectionsOwn(t *testing.T) {
	var meta map[string]any
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			tokenHandler(w)
			return
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		reader := multipart.NewReader(r.Body, params["boundary"])
		part, _ := reader.NextPart()
		body, _ := io.ReadAll(part)
		_ = json.Unmarshal(body, &meta)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"F1","name":"x"}`))
	})
	if _, err := (Drive{}).Send(context.Background(), target("ROOT", ""), integrations.Send{
		Action: "upload",
		File:   &appint.File{Name: "x.csv", Content: []byte("x"), Type: "text/csv"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	parents, _ := meta["parents"].([]any)
	if len(parents) != 1 || parents[0] != "ROOT" {
		t.Errorf("parents = %+v, want the connection's own folder", meta["parents"])
	}
}

// A refused folder read names the address to share with. "Permission denied" on
// its own sends somebody to Google's console with nothing to look for.
func TestVerifyNamesTheAddressToShareWith(t *testing.T) {
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			tokenHandler(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"File not found: ROOT."}}`))
	})
	_, err := Drive{}.Verify(context.Background(), target("ROOT", ""))
	if err == nil {
		t.Fatal("an unshared folder was accepted")
	}
	if !strings.Contains(err.Error(), "toft@example.iam.gserviceaccount.com") {
		t.Errorf("the refusal does not name the address to share with: %v", err)
	}
}

// Every Drive call asks for the full drive scope. `drive.file` reaches only
// files this client created, so the folder an administrator shared with the
// service account — and every subfolder its people made — was invisible to the
// token: a connection that could never verify, with a refusal blaming the
// sharing. Read off the signed assertion, where the scope actually travels.
func TestDriveAsksForTheScopeThatSeesASharedFolder(t *testing.T) {
	var scopes []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_ = r.ParseForm()
			parts := strings.Split(r.PostForm.Get("assertion"), ".")
			if len(parts) == 3 {
				raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
				var claims struct {
					Scope string `json:"scope"`
				}
				_ = json.Unmarshal(raw, &claims)
				scopes = append(scopes, claims.Scope)
			}
			tokenHandler(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ROOT","name":"Finance","files":[]}`))
	})
	if _, err := (Drive{}).Verify(context.Background(), target("ROOT", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := (Drive{}).Destinations(context.Background(), target("ROOT", "")); err != nil {
		t.Fatal(err)
	}
	// One mint: the second call is the cache's.
	if len(scopes) != 1 {
		t.Fatalf("saw %d token requests, want 1", len(scopes))
	}
	for _, s := range scopes {
		if s != "https://www.googleapis.com/auth/drive" {
			t.Errorf("a Drive call asked for %q", s)
		}
	}
}

// Chat's attribution is appended here, not by the app, and the space is the
// app's own choice inside what the Chat app was added to.
func TestChatAppendsAnAttribution(t *testing.T) {
	var body map[string]string
	var path string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			tokenHandler(w)
			return
		}
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := (Chat{}).Send(context.Background(), target("", "spaces/AAAA"),
		integrations.Send{
			Action: "post", Text: "Ingest finished", AppName: "Reconciler",
			AppURL: "https://toft.example/apps/reconciler",
		}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(path, "spaces/AAAA/messages") {
		t.Errorf("posted to %q", path)
	}
	if !strings.Contains(body["text"], "Ingest finished") {
		t.Errorf("the app's own text was lost: %q", body["text"])
	}
	if !strings.Contains(body["text"], "Reconciler") {
		t.Errorf("no attribution: %q", body["text"])
	}
}

// A quota could succeed on a second attempt; "permission denied" could not.
func TestOnlyQuotaAndServerErrorsAreRetryable(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
	} {
		stub(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/token") {
				tokenHandler(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":{"message":"Google's own words"}}`))
		})
		_, err := Drive{}.Send(context.Background(), target("ROOT", ""), integrations.Send{
			Action: "upload",
			File:   &appint.File{Name: "x.csv", Content: []byte("x"), Type: "text/csv"},
		})
		if err == nil {
			t.Fatalf("%d was treated as success", tc.status)
		}
		if got := appint.IsRetryable(err); got != tc.retry {
			t.Errorf("%d retryable = %v, want %v", tc.status, got, tc.retry)
		}
		if !strings.Contains(err.Error(), "Google's own words") {
			t.Errorf("%d lost Google's message: %v", tc.status, err)
		}
	}
}

// The key file's token_uri is never dialled: a pasted document cannot aim the
// signed assertion, or this box's network position, at a URL of its choosing.
// And when the token endpoint refuses, its body is not echoed back.
func TestTokenURIInTheKeyIsIgnoredAndTheBodyNotEchoed(t *testing.T) {
	var elsewhere bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = true
		tokenHandler(w)
	}))
	defer other.Close()

	stub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("internal metadata: AKIA-secret"))
	})

	key, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "toft@example.iam.gserviceaccount.com",
		"private_key":  testKey,
		"token_uri":    other.URL + "/token",
	})
	sa, err := From(integrations.Target{Credentials: map[string]string{"service_account": string(key)}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sa.Token(context.Background(), ScopeDrive)
	if elsewhere {
		t.Fatal("the key's token_uri was dialled")
	}
	if err == nil {
		t.Fatal("a refused token exchange returned no error")
	}
	if strings.Contains(err.Error(), "AKIA-secret") {
		t.Fatalf("the token endpoint's body was echoed: %v", err)
	}
}

// A platform notification carries no live link or mention, and the app name in
// the attribution cannot close the link to the app and open another (RT-10).
func TestAPlatformChatMessageCarriesNoLinkItDidNotCompose(t *testing.T) {
	text := withAttribution(integrations.Send{
		Title:        "<https://evil.example|Payroll>",
		Text:         "failed: <https://evil.example|x> <users/all>",
		AppName:      "Pay> <https://evil.example|x",
		AppURL:       "https://toft.example/apps/payroll",
		FromPlatform: true,
	})
	if strings.Contains(text, "<https://evil") || strings.Contains(text, "<users/all>") {
		t.Errorf("a live link or mention survived: %q", text)
	}
	if n := strings.Count(text, "<"); n != 1 || !strings.Contains(text, "<https://toft.example/apps/payroll|") {
		t.Errorf("text = %q, want exactly one link, to the app", text)
	}
}

// The menus are also the validators, so they page: a folder or a space past the
// first page must still be one an editor can choose.
func TestDriveDestinationsPage(t *testing.T) {
	var queries []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			tokenHandler(w)
			return
		}
		q := r.URL.Query()
		queries = append(queries, q.Get("pageToken"))
		if !strings.Contains(q.Get("fields"), "nextPageToken") {
			t.Errorf("fields %q does not ask for nextPageToken", q.Get("fields"))
		}
		w.Header().Set("Content-Type", "application/json")
		if q.Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"files":[{"id":"F1","name":"Reports"}],"nextPageToken":"p2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"files":[{"id":"F2","name":"Zeta"}]}`))
	})
	got, err := (Drive{}).Destinations(context.Background(), target("ROOT", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != "F2" || len(queries) != 2 || queries[1] != "p2" {
		t.Errorf("destinations %+v after page tokens %q", got, queries)
	}
}

func TestChatDestinationsPage(t *testing.T) {
	var queries []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			tokenHandler(w)
			return
		}
		q := r.URL.Query()
		queries = append(queries, q.Get("pageToken"))
		w.Header().Set("Content-Type", "application/json")
		if q.Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"spaces":[{"name":"spaces/A","displayName":"Ops"}],"nextPageToken":"p2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"spaces":[{"name":"spaces/B","displayName":"Finance"}]}`))
	})
	got, err := (Chat{}).Destinations(context.Background(), target("", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != "spaces/B" || len(queries) != 2 || queries[1] != "p2" {
		t.Errorf("destinations %+v after page tokens %q", got, queries)
	}
}
