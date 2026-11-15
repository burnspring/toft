package mail

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
)

// smtpSink is the least SMTP server that go-mail will send through: it records
// the envelope recipients and the DATA, and accepts everything.
func smtpSink(t *testing.T) (port int, got func() (rcpt []string, data string)) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	done := make(chan struct{})
	var rcpts []string
	var body strings.Builder
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { c.Write([]byte(s + "\r\n")) }
		say("220 sink")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					say("250 ok")
					continue
				}
				body.WriteString(line)
				continue
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 sink")
			case strings.HasPrefix(cmd, "RCPT TO:"):
				rcpts = append(rcpts, strings.Trim(strings.TrimSpace(line[len("RCPT TO:"):]), "<>"))
				say("250 ok")
			case cmd == "DATA":
				inData = true
				say("354 go on")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() ([]string, string) { <-done; return rcpts, body.String() }
}

// BB-7 (2026-11-04): a blind copy is an envelope recipient and nothing else —
// no Bcc: header reaches the people in To and Cc.
func TestBccIsEnvelopeOnly(t *testing.T) {
	port, got := smtpSink(t)
	cfg := Config{Host: "127.0.0.1", Port: port, Security: SecurityNone, FromAddress: "noreply@example.test"}
	err := Send(context.Background(), cfg, Message{
		To: []string{"to@example.test"}, CC: []string{"cc@example.test"}, BCC: []string{"hidden@example.test"},
		Subject: "s", Text: "t", HTML: "<p>t</p>",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	rcpt, data := got()
	if !strings.Contains(strings.Join(rcpt, " "), "hidden@example.test") {
		t.Errorf("envelope recipients %v carry no bcc address", rcpt)
	}
	headers, _, _ := strings.Cut(data, "\r\n\r\n")
	if strings.Contains(strings.ToLower(headers), "bcc:") || strings.Contains(data, "hidden@example.test") {
		t.Errorf("the message itself names the blind copy:\n%s", headers)
	}
}

// BB-8 (2026-11-04): a job that succeeded with notify = "always" arrives on
// the app_event template at severity info, and reads as good news.
func TestAppEventInfoIsNotAFailure(t *testing.T) {
	m, err := Render("app_event", TemplateData{AppName: "nightly-app", EventTitle: `The job "always" succeeded`,
		Severity: "info", Link: "https://x.test/apps/1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{m.Text, m.HTML} {
		if strings.Contains(body, "Something went wrong") || strings.Contains(body, "Every failure") {
			t.Errorf("a success reads as a failure:\n%s", body)
		}
	}
	if !strings.Contains(m.Text, "News from nightly-app") {
		t.Errorf("text = %q", m.Text)
	}
	f, err := Render("app_event", TemplateData{AppName: "a", EventTitle: "crashed", Severity: "error"})
	if err != nil || !strings.Contains(f.Text, "Something went wrong") {
		t.Errorf("a failure lost its wording: %v %q", err, f.Text)
	}
}

// refusingSink accepts any number of connections and answers 550 at RCPT for
// the one address it is told is gone. It counts the messages it took (DATA)
// per envelope recipient.
func refusingSink(t *testing.T, gone string) (port int, delivered func() map[string]int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	got := map[string]int{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				say := func(s string) { c.Write([]byte(s + "\r\n")) }
				say("220 sink")
				var rcpts []string
				inData := false
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if inData {
						if line == ".\r\n" {
							inData = false
							mu.Lock()
							for _, rc := range rcpts {
								got[rc]++
							}
							mu.Unlock()
							rcpts = nil
							say("250 ok")
						}
						continue
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
						say("250 sink")
					case strings.HasPrefix(cmd, "RCPT TO:"):
						addr := strings.Trim(strings.TrimSpace(line[len("RCPT TO:"):]), "<>")
						if addr == gone {
							say("550 5.1.1 no such mailbox")
							continue
						}
						rcpts = append(rcpts, addr)
						say("250 ok")
					case cmd == "DATA":
						inData = true
						say("354 go on")
					case cmd == "RSET":
						rcpts = nil
						say("250 ok")
					case cmd == "QUIT":
						say("221 bye")
						return
					default:
						say("250 ok")
					}
				}
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range got {
			out[k] = v
		}
		return out
	}
}

// VN-2 (2026-11-15): one refused or unparseable recipient no longer loses a
// notification for every other recipient on the list.
func TestOneRefusedRecipientDoesNotSilenceTheOthers(t *testing.T) {
	for _, c := range []struct {
		name string
		bad  string
	}{
		{"refused at RCPT", "gone@example.test"},
		{"unparseable", "a@x.org, b@x.org"},
	} {
		t.Run(c.name, func(t *testing.T) {
			port, delivered := refusingSink(t, "gone@example.test")
			cfg := Config{Host: "127.0.0.1", Port: port, Security: SecurityNone, FromAddress: "noreply@example.test"}
			msg := Message{
				To:      []string{"one@example.test", c.bad, "two@example.test"},
				Subject: "s", Text: "t", HTML: "<p>t</p>",
			}
			ok, failed, err := sendEach(cfg, msg)
			if ok != 2 || failed != 1 || err == nil {
				t.Fatalf("delivered=%d failed=%d err=%v, want 2, 1 and the refusal", ok, failed, err)
			}
			got := delivered()
			for _, want := range []string{"one@example.test", "two@example.test"} {
				if got[want] != 1 {
					t.Errorf("%s received %d messages, want 1 (all: %v)", want, got[want], got)
				}
			}
		})
	}
}

// A failure that is not about the recipient stops the loop: a relay that is
// down is asked once, not once per recipient.
func TestARelayFailureStopsTheRecipientLoop(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens: connection refused
	cfg := Config{Host: "127.0.0.1", Port: port, Security: SecurityNone, FromAddress: "noreply@example.test"}
	ok, failed, err := sendEach(cfg, Message{To: []string{"a@example.test", "b@example.test", "c@example.test"},
		Subject: "s", Text: "t", HTML: "<p>t</p>"})
	if ok != 0 || failed != 3 || err == nil {
		t.Fatalf("delivered=%d failed=%d err=%v, want 0, 3 and an error", ok, failed, err)
	}
	if recipientLevel(err) {
		t.Errorf("a refused connection was taken for a recipient's fault: %v", err)
	}
}
