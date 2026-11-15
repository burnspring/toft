package teams

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/integrations"
	"toft/internal/safefetch"
)

func target(url string) integrations.Target {
	return integrations.Target{
		Connection: database.IntegrationConnection{
			ID: "iconn-1", Kind: "teams", Name: "Build alerts", ScopeID: url,
		},
	}
}

// **An app never speaks in the platform's voice**, and on this transport that
// is enforced by the app supplying a string rather than a card: the attribution
// is a block this side appends, and no amount of markdown in the text can
// delete a block.
func TestEveryCardCarriesAnAttributionTheAppCannotSuppress(t *testing.T) {
	var body []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	tr := Transport{client: safefetch.NewForTesting("", &tls.Config{InsecureSkipVerify: true}, false, Timeout)}
	_, err := tr.Send(context.Background(), target(server.URL), integrations.Send{
		Action:  "post",
		Text:    "Ingest finished",
		AppName: "Reconciler",
		AppURL:  "https://toft.example/apps/reconciler",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var payload struct {
		Attachments []struct {
			Content struct {
				Body []map[string]any `json:"body"`
			} `json:"content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("the payload is not an Adaptive Card: %v", err)
	}
	blocks := payload.Attachments[0].Content.Body
	last, _ := blocks[len(blocks)-1]["text"].(string)
	if !strings.Contains(last, "Reconciler") || !strings.Contains(last, "Toft") {
		t.Errorf("the last block is not the attribution: %q", last)
	}
	// And it is last, so a long message cannot push it out of the card.
	first, _ := blocks[0]["text"].(string)
	if first != "Ingest finished" {
		t.Errorf("the app's own text is not first: %q", first)
	}
}

// The URL is server-side input like every other URL somebody typed here.
func TestTheWorkflowURLGoesThroughTheSameRules(t *testing.T) {
	tr := New()
	for _, raw := range []string{
		"",
		"http://example.com/hook",
		"https://user:pass@example.com/hook",
	} {
		if _, err := tr.Verify(context.Background(), target(raw)); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
	if label, err := tr.Verify(context.Background(),
		target("https://prod-1.westeurope.logic.azure.com/workflows/abc/triggers/manual")); err != nil {
		t.Errorf("a real-looking workflow URL was refused: %v", err)
	} else if label == "" {
		t.Error("a verified connection got no scope label")
	}
}

// **A URL somebody typed is refused at save as well as at dial** when it points
// inward (docs/invariants.md § The integration catalog). Literal addresses and
// a name that resolves to one, which is the case string inspection misses.
func TestAPrivateWorkflowAddressIsRefusedAtSave(t *testing.T) {
	tr := New()
	for _, raw := range []string{
		"https://10.0.0.5/workflows/abc/triggers/manual/paths/invoke?sig=x",
		"https://127.0.0.1/invoke?sig=x",
		"https://169.254.169.254/latest/meta-data/",
		"https://localhost/workflows/abc",
	} {
		_, err := tr.Verify(context.Background(), target(raw))
		if err == nil {
			t.Errorf("%q was accepted at save", raw)
			continue
		}
		if !errors.Is(err, safefetch.ErrPrivateAddress) {
			t.Errorf("%q was refused for the wrong reason: %v", raw, err)
		}
		if strings.Contains(err.Error(), "sig=") {
			t.Errorf("the refusal repeats the URL's signature: %v", err)
		}
	}
}

// **The query string is the credential**, so it is split off at save: the
// scope keeps scheme, host and path, the signature is sealed, and the
// transport alone puts them back together.
func TestTheSignatureIsSplitOffTheScopeAndRejoinedOnlyToSend(t *testing.T) {
	const pasted = "https://default81.6a.environment.api.powerplatform.com:443/powerautomate/" +
		"automations/direct/workflows/166331419bd741f181eb28bd825992f4/triggers/manual/" +
		"paths/invoke?api-version=1&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=SECRET"
	scope, secrets, err := Transport{}.SplitScope(pasted)
	if err != nil {
		t.Fatalf("SplitScope: %v", err)
	}
	if strings.Contains(scope, "SECRET") || strings.Contains(scope, "?") {
		t.Errorf("the scope keeps the query: %q", scope)
	}
	if !strings.HasSuffix(scope, "/paths/invoke") {
		t.Errorf("the scope lost its path: %q", scope)
	}
	if !strings.Contains(secrets[QueryKey], "sig=SECRET") {
		t.Errorf("the signature is not in the sealed fields: %v", secrets)
	}

	split := target(scope)
	split.Credentials = secrets
	if got := Endpoint(split); got != pasted {
		t.Errorf("Endpoint = %q, want the URL as pasted", got)
	}
	// A row saved before the split holds the whole URL and no sealed query.
	if got := Endpoint(target(pasted)); got != pasted {
		t.Errorf("a pre-split row posts to %q, not what it holds", got)
	}
}

// The label is the workflow's id, which is what Power Automate shows — so two
// Teams connections no longer read as the same "a Teams workflow".
func TestTheLabelNamesTheWorkflow(t *testing.T) {
	cases := map[string]string{
		"https://x.powerplatform.com/powerautomate/automations/direct/workflows/166331419bd741f181eb28bd825992f4/triggers/manual": "workflow 1663…92f4",
		"https://prod-1.westeurope.logic.azure.com/workflows/abc/triggers/manual":                                                 "workflow abc",
		"https://hooks.example.com/teams": "a Teams workflow on hooks.example.com",
	}
	for scope, want := range cases {
		if got := Label(scope); got != want {
			t.Errorf("Label(%q) = %q, want %q", scope, got, want)
		}
	}
}

// net/http's *url.Error prints the whole request URL, and that string becomes
// the ledger's error and a log line. Nothing a send returns may carry `sig`.
func TestATransportErrorNeverCarriesTheSignature(t *testing.T) {
	tr := Transport{client: safefetch.NewForTesting("", &tls.Config{}, false, 50*time.Millisecond)}
	// TEST-NET-1: routable, never answers, so the dial times out.
	tgt := target("https://192.0.2.1/workflows/abc/triggers/manual/paths/invoke")
	tgt.Credentials = map[string]string{QueryKey: "api-version=1&sig=SECRET"}
	_, err := tr.Send(context.Background(), tgt, integrations.Send{Action: "post", Text: "x"})
	if err == nil {
		t.Fatal("a send to an address that never answers succeeded")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "sig=") {
		t.Errorf("the error carries the signature: %v", err)
	}
}

// A platform notification's title and text are TextRuns, which parse no
// markdown, and the attribution carries exactly one link, to the app (RT-10).
func TestAPlatformCardCarriesNoLinkItDidNotCompose(t *testing.T) {
	card := Card(integrations.Send{
		Title:        "[Payroll](https://evil.example)",
		Text:         "build failed: [Sign in again](https://evil.example)",
		AppName:      "Pay](https://evil.example) [x",
		AppURL:       "https://toft.example/apps/payroll",
		FromPlatform: true,
	})
	blocks := card["attachments"].([]map[string]any)[0]["content"].(map[string]any)["body"].([]map[string]any)
	for _, b := range blocks[:len(blocks)-1] {
		if b["type"] != "RichTextBlock" {
			t.Errorf("a platform block is a %v, which parses markdown", b["type"])
		}
	}
	attribution, _ := blocks[len(blocks)-1]["text"].(string)
	if n := strings.Count(attribution, "]("); n != 1 || !strings.Contains(attribution, "](https://toft.example/apps/payroll)") {
		t.Errorf("attribution = %q, want exactly one link, to the app", attribution)
	}
}
