package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --output writes the whole body, unbounded and byte for byte, and a refusal
// leaves an existing file as it was.
func TestFetchOutputWritesTheBodyAndKeepsAGoodFileOnRefusal(t *testing.T) {
	binary := append(bytes.Repeat([]byte{0, 0xff, 'x'}, 10000), '\n')
	e, stdout, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "no such report")
			return
		}
		w.Write(binary)
	}))
	out := filepath.Join(t.TempDir(), "report.bin")
	if err := cmdFetch(e, []string{"/report", "--app", "demo", "-o", out}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, binary) {
		t.Fatalf("the file is %d bytes (want %d): %v", len(got), len(binary), err)
	}
	if strings.Contains(stdout.String(), "\xff") {
		t.Error("the body was printed as well as written")
	}

	stdout.Reset()
	if err := cmdFetch(e, []string{"/missing", "--app", "demo", "--output", out, "--force"}); err == nil {
		t.Fatal("a 404 succeeded")
	}
	if again, _ := os.ReadFile(out); !bytes.Equal(again, binary) {
		t.Error("a refusal overwrote the file")
	}
	if !strings.Contains(stdout.String(), "no such report") {
		t.Errorf("the refusal was not printed: %q", stdout)
	}
	entries, _ := os.ReadDir(filepath.Dir(out))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// --form sends multipart, a file by its own name, and implies POST.
func TestFetchFormSendsMultipart(t *testing.T) {
	var method, field, fileName, fileBody string
	e, _, _ := splitInstall(t, appOrigin(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("not multipart: %v", err)
			return
		}
		field = r.FormValue("kind")
		f, h, err := r.FormFile("upload")
		if err != nil {
			t.Errorf("no file: %v", err)
			return
		}
		b, _ := io.ReadAll(f)
		fileName, fileBody = h.Filename, string(b)
	}))
	src := filepath.Join(t.TempDir(), "frs.csv")
	os.WriteFile(src, []byte("a,b\n1,2\n"), 0o600)
	if err := cmdFetch(e, []string{"/load", "--app", "demo", "--form", "kind=frs", "--form", "upload=@" + src}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || field != "frs" || fileName != "frs.csv" || fileBody != "a,b\n1,2\n" {
		t.Errorf("got %s kind=%q file=%q %q", method, field, fileName, fileBody)
	}
	if err := cmdFetch(e, []string{"/load", "--app", "demo", "--form", "k=v", "--data", "x"}); err == nil {
		t.Error("--form with --data was accepted")
	}
}
