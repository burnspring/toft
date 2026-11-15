package slack

import (
	"toft/internal/integrations"
	"toft/internal/notify"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// App-initiated Slack posts: a message an *app* wrote.
//
// The rules that shape this are the ones ADR §25 locks, and two
// of them are visible in the signatures here.
//
// **App code never names a channel.** PostForApp takes one, but it comes from
// database.ResolveAttachment and from nowhere else — the app's owners choose
// it in the UI from the channels somebody invited the bot into. If app code
// could name a channel, "an app may post to exactly the channels somebody
// invited the bot into" would degrade to "an app may post anywhere the bot is",
// which is a different and much weaker sentence.
//
// **An app never posts in the platform's voice.** Every post here carries a
// context line naming the app and linking to it, composed on this side and not
// reachable from anything app code sent. And app text is mrkdwn only, never
// arbitrary Block Kit: a block tree can reproduce the platform's own alert
// formatting field for field, and there is no cheap validator for "does not
// look like Toft".

// appTextLimit is Slack's own limit on a section's text. Exceeding it is a
// refusal at the handler rather than a truncation here: an app that silently
// loses the tail of its report has a report that is wrong in a way nobody
// notices.
const appTextLimit = 3000

// APIError is a failure Slack itself reported, carrying enough to decide
// whether trying again could possibly help.
//
// It answers Retryable() rather than being wrapped by the caller so that the
// judgement lives with the transport that knows Slack's vocabulary. appint
// reads it through a bare interface, which is why nothing here imports appint.
type APIError struct {
	Method string
	Code   string
	Status int
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("Slack refused %s: %s", e.Method, e.Code)
	}
	return fmt.Sprintf("Slack returned HTTP %d from %s", e.Status, e.Method)
}

// Retryable reports whether a second attempt could succeed. Rate limits and
// server errors could; "channel_not_found" and "not_in_channel" could not, and
// retrying them only doubles the log noise for a problem a person must fix.
func (e *APIError) Retryable() bool {
	if e.Status == http.StatusTooManyRequests || e.Status/100 == 5 {
		return true
	}
	switch e.Code {
	case "ratelimited", "rate_limited", "service_unavailable", "internal_error", "fatal_error":
		return true
	}
	return false
}

// PostForApp posts one app-written message.
//
// text is Slack mrkdwn and is used as-is. It is deliberately *not* escaped:
// mrkdwn is the formatting an app author is writing in, and escaping it would
// make `*bold*` arrive as literal asterisks. What that costs is that an app can
// post a link — which it could do in plain text anyway — and what it cannot do
// is forge the attribution, because that is a separate block this function
// appends and no amount of mrkdwn can delete a block.
//
// platform is the install's name, or "" for "Toft", set by the platform and
// never from the app's request.
func PostForApp(ctx context.Context, cfg Config, channel, text, appName, appURL, platform string) error {
	if strings.TrimSpace(channel) == "" {
		return errors.New("no channel")
	}
	blocks := []Block{
		{Type: "section", Text: &Text{Type: "mrkdwn", Text: text}},
		appContext(appName, appURL, platform),
	}
	body := map[string]interface{}{
		"channel": channel,
		// The fallback Slack shows in the sidebar and in a desktop toast. It
		// names the app for the same reason the context block does: a
		// notification that reads only "1,204 rows" tells nobody which app
		// produced it.
		"text":   fmt.Sprintf("[%s] %s", appName, text),
		"blocks": blocks,
	}
	_, err := call(ctx, cfg, "chat.postMessage", body)
	return classify("chat.postMessage", err)
}

// UploadForApp puts a file in the app's channel.
//
// files.upload is deprecated and gone, so this is the two-step external upload:
// ask Slack for a URL, PUT the bytes at it, then tell Slack the upload is
// complete and which channel it belongs to. It needs the files:write scope, and
// a workspace whose bot lacks it gets Slack's own error text — the standing
// rule that the sender's error reaches the administrator rather than being
// flattened into "upload failed".
func UploadForApp(ctx context.Context, cfg Config, channel, filename string,
	content []byte, title, comment, appName, appURL, platform string) error {

	if strings.TrimSpace(channel) == "" {
		return errors.New("no channel")
	}
	if len(content) == 0 {
		return errors.New("the file is empty")
	}
	if title == "" {
		title = filename
	}

	// 1. Where to put it.
	raw, err := callForm(ctx, cfg, "files.getUploadURLExternal", url.Values{
		"filename": {filename},
		"length":   {strconv.Itoa(len(content))},
	})
	if err != nil {
		return classify("files.getUploadURLExternal", err)
	}
	var ticket struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := json.Unmarshal(raw, &ticket); err != nil || ticket.UploadURL == "" || ticket.FileID == "" {
		return errors.New("Slack did not return an upload location")
	}

	// 2. The bytes. Not through call(): this goes to Slack's file host rather
	// than the API host, and it carries no bearer token — the ticket is the
	// authorization.
	if err := putFile(ctx, ticket.UploadURL, filename, content); err != nil {
		return err
	}

	// 3. Publish it into the channel, with the attribution the app cannot
	// suppress folded into the comment.
	files, _ := json.Marshal([]map[string]string{{"id": ticket.FileID, "title": title}})
	body := map[string]interface{}{
		"files":           string(files),
		"channel_id":      channel,
		"initial_comment": appComment(comment, appName, appURL, platform),
	}
	_, err = call(ctx, cfg, "files.completeUploadExternal", body)
	return classify("files.completeUploadExternal", err)
}

// appContext is the attribution block. Composed here and appended after the
// app's own content, so nothing app code sends can remove or forge it. The
// platform it names is the install's name, or "Toft".
func appContext(appName, appURL, platform string) Block {
	return Block{Type: "context", Elements: []Text{{Type: "mrkdwn", Text: appAttribution(appName, appURL, platform)}}}
}

// appAttribution is the sentence both attributions carry.
func appAttribution(appName, appURL, platform string) string {
	on := esc(notify.SenderOrDefault(platform))
	if appURL != "" {
		return fmt.Sprintf("Sent by <%s|%s>, an application on %s", esc(appURL), esc(appName), on)
	}
	return fmt.Sprintf("Sent by *%s*, an application on %s", esc(appName), on)
}

// appComment carries the same attribution on a file post, where there are no
// blocks to append one to.
func appComment(comment, appName, appURL, platform string) string {
	attribution := appAttribution(appName, appURL, platform)
	if strings.TrimSpace(comment) == "" {
		return attribution
	}
	return comment + "\n\n" + attribution
}

// callForm posts a form-encoded request. files.getUploadURLExternal and
// users.conversations do not accept a JSON body — a method that does not take
// JSON ignores it — which is the only reason this exists beside call().
func callForm(ctx context.Context, cfg Config, method string, form url.Values) ([]byte, error) {
	base := apiBase
	if cfg.baseURL != "" {
		base = cfg.baseURL
	}
	endpoint, err := url.JoinPath(base, method)
	if err != nil {
		return nil, fmt.Errorf("building the Slack request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("building the Slack request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Slack: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))

	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Method: method, Status: resp.StatusCode}
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("Slack returned something that is not JSON (HTTP %d)", resp.StatusCode)
	}
	if !envelope.OK {
		return nil, &APIError{Method: method, Code: envelope.Error, Status: resp.StatusCode}
	}
	return raw, nil
}

func putFile(ctx context.Context, uploadURL, filename string, content []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("building the upload: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return fmt.Errorf("building the upload: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("building the upload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &buf)
	if err != nil {
		return fmt.Errorf("building the upload: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := httpClient.Do(req)
	if err != nil {
		// The upload ticket is in the path, and *url.Error's text is the URL.
		return fmt.Errorf("uploading to Slack: %w", integrations.RedactURL(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return &APIError{Method: "file upload", Status: resp.StatusCode}
	}
	return nil
}

// classify turns the plain errors call() returns into an APIError where it can,
// so the queue's one retry is spent on failures that might not repeat.
func classify(method string, err error) error {
	if err == nil {
		return nil
	}
	var api *APIError
	if errors.As(err, &api) {
		return err
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "429"), strings.Contains(text, "rate-limiting"):
		return &APIError{Method: method, Status: http.StatusTooManyRequests}
	case strings.Contains(text, "HTTP 5"):
		return &APIError{Method: method, Status: http.StatusInternalServerError}
	}
	return err
}
