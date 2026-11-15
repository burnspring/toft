package msfiles

import (
	"context"
	"testing"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations"
)

func target(scope string) integrations.Target {
	return integrations.Target{
		Connection: database.IntegrationConnection{
			ID: "iconn-1", Kind: "sharepoint", Name: "Finance", ScopeID: scope,
		},
		Credentials: map[string]string{"client_secret": "s"},
		Destination: database.Destination{ID: "FOLDER", Label: "Reports"},
	}
}

// A name that is really a path is refused **before** anything is dialled.
//
// The order matters: a sanitiser would have uploaded something, under a name
// the app cannot find again, and told it the send succeeded. This refuses
// without a round trip, which is also why the test needs no Graph at all.
func TestAPathForANameIsRefusedBeforeAnythingIsDialled(t *testing.T) {
	tr := SharePoint()
	for _, name := range []string{"reports/june.csv", `..\june.csv`, ".hidden", "CON"} {
		_, err := tr.Send(context.Background(), target("contoso.sharepoint.com/sites/finance"),
			integrations.Send{
				Action: "upload",
				File:   &appint.File{Name: name, Content: []byte("x"), Type: "text/csv"},
			})
		if err == nil {
			t.Errorf("%q was accepted as a filename", name)
		}
	}
}

// A file action with no file is a refusal rather than an empty upload.
func TestAFileActionNeedsAFile(t *testing.T) {
	_, err := SharePoint().Send(context.Background(), target("contoso.sharepoint.com/sites/x"),
		integrations.Send{Action: "upload"})
	if err == nil {
		t.Fatal("an upload with no file was accepted")
	}
}

// An empty scope is refused at save with the sentence that says what to paste,
// and the two kinds say different things because they take different scopes.
func TestVerifyNamesWhatItNeeds(t *testing.T) {
	if _, err := SharePoint().Verify(context.Background(), target("")); err == nil {
		t.Error("a SharePoint connection with no site was accepted")
	} else if got := err.Error(); got == "" {
		t.Error("the refusal has no words")
	}
	if _, err := OneDrive().Verify(context.Background(), target("")); err == nil {
		t.Error("a OneDrive connection with no account was accepted")
	}
}

// The two are one transport with a flag, and the flag is the only difference.
// If that stops being true, this is where it shows.
func TestTheTwoDifferOnlyInWhichDrive(t *testing.T) {
	if SharePoint().personal {
		t.Error("SharePoint writes to a site's library, not a person's drive")
	}
	if !OneDrive().personal {
		t.Error("OneDrive writes to a person's drive")
	}
}
