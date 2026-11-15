package api

import (
	"testing"

	"toft/internal/appint"
)

// A file-only kind can do exactly one thing, so its test does that thing. The
// test used to send a text payload, which SharePoint, OneDrive and Google
// Drive refuse — "this integration takes a file" — after a 202 and a toast
// saying the test had been sent, so a correctly configured connection could
// never pass its own test.
func TestAFileKindsTestSendsAFile(t *testing.T) {
	fileKinds := 0
	for _, k := range appint.Known {
		if k.Actions[0].Carries != appint.CarriesFile {
			continue
		}
		fileKinds++
	}
	if fileKinds == 0 {
		t.Fatal("the catalog has no file-only kind; the test has nothing to prove")
	}
	f := testFile("hello")
	if f.Name != "toft-test.txt" || f.Type != "text/plain" || string(f.Content) != "hello\n" {
		t.Errorf("testFile = %+v, want a one-line toft-test.txt", f)
	}
}
