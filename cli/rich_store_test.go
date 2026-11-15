package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/cli/internal/client"
)

// On a terminal a store listing is a table: a header, sizes right-aligned so
// magnitudes line up, an age rather than a timestamp, and a folder with no size
// or date invented for it. The path stays whole — it is what the next command
// takes.
func TestStoreListIsATableOnATerminal(t *testing.T) {
	modified := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fmt.Sprintf(`{"objects":[{"name":"sales.db","size":84000,"last_modified":%q},`+
			`{"name":"a.txt","size":12,"last_modified":%q}],"prefixes":["backup/"]}`, modified, modified))
	}))
	defer srv.Close()
	s := &session{client: client.New(srv.URL, "tok", "toft-test")}

	e, buf := testEnv()
	e.out.Interactive = true
	e.out.Columns = 100
	if err := listObjects(e, s, &globalFlags{}, mustRemote(t, "drop"), false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "SIZE") {
		t.Fatalf("want a header and three rows:\n%s", buf.String())
	}
	for _, want := range []string{"toft://drop/backup/", "toft://drop/sales.db", "3h ago", "82.0 KB"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %q in:\n%s", want, buf.String())
		}
	}
	// Right-aligned: the two sizes end in the same column.
	if strings.Index(lines[2], "KB")+2 != strings.Index(lines[3], " B")+2 {
		t.Errorf("sizes are not right-aligned:\n%s\n%s", lines[2], lines[3])
	}
	if !strings.Contains(lines[1], "—") {
		t.Errorf("a folder should show no size: %q", lines[1])
	}
}

// Off a terminal the listing is the lines it always was: no header.
func TestStoreListPlainHasNoHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"objects":[{"name":"sales.db","size":84000,"last_modified":"2026-09-03T01:57:00Z"}]}`)
	}))
	defer srv.Close()
	s := &session{client: client.New(srv.URL, "tok", "toft-test")}
	e, buf := testEnv()
	if err := listObjects(e, s, &globalFlags{}, mustRemote(t, "drop"), false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "SIZE") || !strings.HasPrefix(buf.String(), "     82.0 KB  2026-09-03") {
		t.Errorf("the plain listing moved:\n%q", buf.String())
	}
}

// The status line is coloured, never reworded.
func TestStatusToneKeepsTheWords(t *testing.T) {
	e, _ := testEnv()
	for code, want := range map[int]string{200: "200 OK", 302: "302 Found", 404: "404 Not Found", 502: "502 Bad Gateway"} {
		if got := statusTone(e, code); got != want {
			t.Errorf("statusTone(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestUntilWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Hour:           "expired",
		30 * time.Minute:     "in under an hour",
		5*time.Hour + 30e9:   "in 5 hours",
		6*24*time.Hour + 1e9: "in 6 days",
	} {
		if got := untilWords(time.Now().Add(d)); got != want {
			t.Errorf("untilWords(%v) = %q, want %q", d, got, want)
		}
	}
}
