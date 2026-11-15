package builder

import (
	"os"
	"path/filepath"
	"testing"
)

// The warning goes to an app that does not read HOST and PORT, and only to one.
func TestReadsHostAndPortIsReadFromTheSource(t *testing.T) {
	cases := []struct {
		name, file, src string
		ext             string
		want            bool
	}{
		{"go reads both", "main.go", `addr := os.Getenv("HOST") + ":" + os.Getenv("PORT")`, ".go", true},
		{"go hard-codes", "main.go", `http.ListenAndServe(":8080", nil) // PORT`, ".go", false},
		{"go port only", "main.go", `os.Getenv("PORT")`, ".go", false},
		{"node", "server.js", `app.listen(process.env.PORT, process.env.HOST)`, ".js", true},
		{"python", "app.py", `serve(host=os.environ["HOST"], port=int(os.environ["PORT"]))`, ".py", true},
		{"a test file is not the app", "main_test.go", `os.Getenv("HOST"); os.Getenv("PORT")`, ".go", false},
	}
	for _, c := range cases {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, c.file), []byte(c.src), 0o600)
		if got := readsHostAndPort(dir, c.ext); got != c.want {
			t.Errorf("%s = %v, want %v", c.name, got, c.want)
		}
	}

	// What vendor/ and node_modules/ say is not what the app does.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "vendor", "x"), 0o700)
	os.WriteFile(filepath.Join(dir, "vendor", "x", "x.go"), []byte(`os.Getenv("HOST"); os.Getenv("PORT")`), 0o600)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`http.ListenAndServe(":8080", nil)`), 0o600)
	if readsHostAndPort(dir, ".go") {
		t.Error("vendored code counted as the app's")
	}
}
