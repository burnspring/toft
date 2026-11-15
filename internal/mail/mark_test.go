package mail

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every message with HTML draws the Toft mark — the platform's in its header,
// an app's in the footer that says an application wrote it — and carries the
// PNG inline, so the image shows on open in clients that block linked ones.
func TestEveryHTMLMessageCarriesTheMarkInline(t *testing.T) {
	platform, err := Render("test_email", TemplateData{BaseURL: "https://toft.example"})
	if err != nil {
		t.Fatal(err)
	}
	app := BuildAppMessage("", "Reports", "https://apps.example/apps/reports", "", "Nightly", "text", "<p>hi</p>",
		[]string{"a@example.com"}, nil, nil, nil)
	for name, msg := range map[string]Message{"platform": platform, "app": app} {
		if !usesMark(msg.HTML) {
			t.Errorf("%s message does not draw the mark", name)
		}
	}
	if plain := BuildAppMessage("", "Reports", "", "", "s", "text only", "", []string{"a@example.com"}, nil, nil, nil); usesMark(plain.HTML) {
		t.Error("a plain-text app message draws the mark")
	}

	var got struct {
		Attachments []resendAttachment `json:"attachments"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	if err := sendAPI(t.Context(), apiConfig(srv.URL), app); err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].ContentID != markName ||
		got.Attachments[0].ContentType != "image/png" || len(got.Attachments[0].Content) != len(markPNG) {
		t.Errorf("attachments = %+v, want the mark inline as %s", got.Attachments, markName)
	}
	if !strings.HasPrefix(string(markPNG), "\x89PNG") {
		t.Error("the embedded mark is not a PNG")
	}
}
