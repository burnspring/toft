package mail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
)

func apiConfig(srvURL string) Config {
	return Config{
		Transport: TransportAPI, APIProvider: ProviderResend, APIKey: "re_test_key",
		FromAddress: "toft@example.com", FromName: "Toft",
		apiBaseURL: srvURL,
	}
}

func TestAPIConfigValidate(t *testing.T) {
	valid := apiConfig("")

	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid"},
		{name: "no key", mutate: func(c *Config) { c.APIKey = " " }, wantErr: "API key is required"},
		{name: "key with newline", mutate: func(c *Config) { c.APIKey = "re_key\n" }, wantErr: "space or line break"},
		{name: "unknown provider", mutate: func(c *Config) { c.APIProvider = "sesame" }, wantErr: "unknown email API provider"},
		{name: "no from", mutate: func(c *Config) { c.FromAddress = "" }, wantErr: "From address is required"},
		// The relay half is irrelevant to an API send and must not be demanded.
		{name: "no smtp host is fine", mutate: func(c *Config) { c.Host, c.Port = "", 0 }},
		{name: "unknown transport", mutate: func(c *Config) { c.Transport = "carrier-pigeon" }, wantErr: "unknown email transport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSendAPIPostsTheMessage(t *testing.T) {
	type captured struct {
		auth        string
		contentType string
		body        resendRequest
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got.body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c7e1"}`))
	}))
	defer srv.Close()

	cfg := apiConfig(srv.URL)
	cfg.ReplyTo = "ops@example.com"
	msg := Message{
		To: []string{"ada@example.com"}, Subject: "Publisher access requested",
		HTML: "<p>hi</p>", Text: "hi", Audience: AudienceAdmin,
	}
	if err := Send(context.Background(), cfg, msg); err != nil {
		t.Fatalf("Send() = %v, want nil", err)
	}

	if got.auth != "Bearer re_test_key" {
		t.Errorf("Authorization = %q, want a bearer token", got.auth)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q", got.contentType)
	}
	if got.body.From != `"Toft" <toft@example.com>` {
		t.Errorf("from = %q, want the display name quoted", got.body.From)
	}
	if len(got.body.To) != 1 || got.body.To[0] != "ada@example.com" {
		t.Errorf("to = %v", got.body.To)
	}
	if got.body.Subject != msg.Subject {
		t.Errorf("subject = %q", got.body.Subject)
	}
	// Both bodies travel, for the same reason the SMTP path sends both.
	if got.body.HTML != "<p>hi</p>" || got.body.Text != "hi" {
		t.Errorf("bodies = %q / %q", got.body.HTML, got.body.Text)
	}
	if got.body.ReplyTo != "ops@example.com" {
		t.Errorf("reply_to = %q", got.body.ReplyTo)
	}
}

// App mail over the API carries everything the SMTP path does: the app's
// "(via Toft)" display name on the install's address, the app's Reply-To, its
// CC and BCC, and its attachments. This transport once sent none of them while
// the ledger recorded the message as delivered.
func TestSendAPICarriesAppMailWhole(t *testing.T) {
	var got resendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := apiConfig(srv.URL)
	cfg.ReplyTo = "ops@example.com"
	msg := Message{
		To: []string{"ada@example.com"}, CC: []string{"cc@example.com"}, BCC: []string{"bcc@example.com"},
		Subject: "Nightly report", HTML: "<p>r</p>", Text: "r", Audience: AudienceAdmin,
		FromName: "Reports" + AppDisplaySuffix(""), ReplyTo: "team@example.com",
		Attachments: []Attachment{{Name: "r.csv", Type: "text/csv", Content: []byte("a,b\n")}},
	}
	if err := Send(context.Background(), cfg, msg); err != nil {
		t.Fatalf("Send() = %v", err)
	}

	if want := (&mail.Address{Name: "Reports" + AppDisplaySuffix(""), Address: "toft@example.com"}).String(); got.From != want {
		t.Errorf("from = %q, want %q — the app's name on the install's address", got.From, want)
	}
	if got.ReplyTo != "team@example.com" {
		t.Errorf("reply_to = %q, want the app's own", got.ReplyTo)
	}
	if len(got.CC) != 1 || got.CC[0] != "cc@example.com" || len(got.BCC) != 1 || got.BCC[0] != "bcc@example.com" {
		t.Errorf("cc/bcc = %v / %v", got.CC, got.BCC)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Filename != "r.csv" ||
		string(got.Attachments[0].Content) != "a,b\n" || got.Attachments[0].ContentType != "text/csv" {
		t.Errorf("attachments = %+v", got.Attachments)
	}
}

// An administrator debugging a mail path needs the provider's own words, the way
// the SMTP path gives them "535 authentication failed".
func TestSendAPISurfacesProviderError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{
			name: "validation error", status: http.StatusForbidden,
			body: `{"statusCode":403,"name":"validation_error","message":"The example.com domain is not verified."}`,
			want: []string{"Resend", "domain is not verified", "403"},
		},
		{
			name: "bad key", status: http.StatusUnauthorized,
			body: `{"statusCode":401,"name":"validation_error","message":"API key is invalid"}`,
			want: []string{"rejected the API key", "401"},
		},
		{
			name: "rate limited", status: http.StatusTooManyRequests,
			body: `{"message":"Too many requests. Please try again later."}`,
			want: []string{"Too many requests", "429"},
		},
		{
			name: "not even json", status: http.StatusBadGateway,
			body: `<html>upstream is sad</html>`,
			want: []string{"upstream is sad", "502"},
		},
		{
			name: "empty body", status: http.StatusInternalServerError,
			body: ``,
			want: []string{"Internal Server Error", "500"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := Send(context.Background(), apiConfig(srv.URL), Message{
				To: []string{"ada@example.com"}, Subject: "s", Text: "t", Audience: AudienceAdmin})
			if err == nil {
				t.Fatal("Send() = nil, want an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "re_test_key") {
				t.Error("the error text carries the API key")
			}
		})
	}
}

// A From address with a comma in the display name must not be able to split the
// header — the API path formats it through net/mail for exactly this reason.
func TestSendAPIQuotesTheDisplayName(t *testing.T) {
	cfg := apiConfig("")
	cfg.FromName = "Toft, Platform Team"
	got := formatFrom(cfg, Message{})
	want := `"Toft, Platform Team" <toft@example.com>`
	if got != want {
		t.Errorf("formatFrom() = %q, want %q", got, want)
	}
	cfg.FromName = ""
	if got := formatFrom(cfg, Message{}); got != "toft@example.com" {
		t.Errorf("formatFrom() with no name = %q", got)
	}
}
