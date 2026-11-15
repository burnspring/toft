package appint

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/notify"
	"toft/internal/telemetry"
)

// The drift this catalog exists to remove is exactly the drift nobody sees: a
// kind that renders in the admin UI, attaches cleanly and then refuses with an
// empty string. Nothing errors, nothing logs, and the first person to find out
// is a developer reading a 403 with no body.
//
// So this test walks Known and insists every field a downstream reader depends
// on is populated. The registry half — that every kind has a transport — is
// asserted in internal/api, where the registry is; a kind can be complete here
// and still have nowhere to go, and that is a different silence with its own
// test.
func TestEveryKindIsCompletelyRegistered(t *testing.T) {
	// The telemetry budget is a bound on the catalog's *length*, declared in a
	// package that deliberately does not import this one. A kind past it is a
	// label whose cardinality nobody argued for.
	if len(Known) > telemetry.IntegrationKindBound {
		t.Errorf("the catalog has %d kinds and telemetry budgets for %d — raise the bound "+
			"with a paragraph saying why, or fold a kind", len(Known), telemetry.IntegrationKindBound)
	}

	seen := map[string]bool{}
	for _, k := range Known {
		t.Run(k.Name, func(t *testing.T) {
			switch {
			case k.Name == "":
				t.Fatal("a kind with no name")
			case seen[k.Name]:
				t.Fatalf("%q appears twice in Known", k.Name)
			case k.Title == "":
				t.Error("no title — the connection picker renders one per kind")
			case k.Summary == "":
				t.Error("no summary")
			case k.ScopeLabel == "":
				t.Error("no scope label — the connection form asks for a scope by name")
			}
			seen[k.Name] = true

			// All three refusals. A gate that refuses with "" is a 403 with no
			// body, which is the failure this whole file is about.
			for label, sentence := range map[string]string{
				"not connected": k.Refusals.NotConnected,
				"not attached":  k.Refusals.NotAttached,
				"disabled":      k.Refusals.Disabled,
			} {
				if strings.TrimSpace(sentence) == "" {
					t.Errorf("no refusal copy for the %q gate", label)
				}
			}
			// The three sentences must differ. Three gates with one sentence is
			// three fixes described as one, which is the same defect wearing
			// prose.
			if k.Refusals.NotConnected == k.Refusals.NotAttached ||
				k.Refusals.NotAttached == k.Refusals.Disabled {
				t.Error("two gates say the same thing — they have different fixes")
			}

			if len(k.Actions) == 0 {
				t.Error("no actions — an integration an app cannot do anything with")
			}
			if k.TextLimit <= 0 {
				t.Error("no text limit — every limit is a refusal that names a number")
			}
			actions := map[string]bool{}
			for _, a := range k.Actions {
				switch {
				case a.Name == "":
					t.Error("an action with no name")
				case actions[a.Name]:
					t.Errorf("action %q appears twice", a.Name)
				case a.Carries == CarriesNothing && a.Name != ActionToken:
					t.Errorf("action %q carries nothing and is not the token action", a.Name)
				case a.Carries != CarriesText && a.Carries != CarriesFile &&
					a.Carries != CarriesNothing:
					t.Errorf("action %q carries %q, which is neither text nor a file",
						a.Name, a.Carries)
				case a.Summary == "":
					t.Errorf("action %q has no summary — the attach dialog reads it", a.Name)
				case a.Carries == CarriesFile && k.FileLimit <= 0:
					t.Errorf("action %q carries a file and the kind has no file limit", a.Name)
				}
				actions[a.Name] = true
			}

			// A kind that enumerates needs the noun the picker is labelled
			// with; one that does not must not claim a destination route it
			// will 404.
			if k.Enumerates && k.DestinationLabel == "" {
				t.Error("enumerates destinations and has no word for one")
			}

			// Audiences come from notify's vocabulary or they are nothing: a
			// typo here is a platform notification that is silently never
			// posted.
			for _, a := range k.PlatformAudiences {
				switch a {
				case notify.AudienceAdmin, notify.AudienceApp:
				default:
					t.Errorf("platform audience %q is not one notify knows", a)
				}
			}

			// A file kind carries no platform audience. "Your app crashed" as a
			// text file in a document library is a notification nobody reads,
			// and this is the data answer rather than a special case.
			if k.LeavesResidue && len(k.PlatformAudiences) > 0 {
				t.Error("a residue-leaving kind serves a platform audience — " +
					"a crash notice does not belong in a document library")
			}
		})
	}
}

// The catalog's central promise: the four gates behave identically for every
// kind, with distinct codes. This is what "the gate chain is one chain" means
// as behaviour rather than as a claim about the code's shape.
func TestCheckIsIdenticalAcrossKinds(t *testing.T) {
	open := database.Attachment{Connected: true, Attached: true, Enabled: true, IssueTokens: true}

	for _, k := range Known {
		for _, a := range k.Actions {
			capability := k.Capability(a.Name)

			cases := []struct {
				name   string
				attach database.Attachment
				status string
				code   string
				want   int
			}{
				{"not connected", database.Attachment{}, "",
					k.Name + "_off", http.StatusForbidden},
				{"not attached", database.Attachment{Connected: true}, "",
					k.Name + "_not_attached", http.StatusForbidden},
				{"disabled", database.Attachment{Connected: true, Attached: true}, "",
					k.Name + "_disabled", http.StatusForbidden},
				{"stopped", open, database.AppStatusStopped,
					"app_stopped", http.StatusConflict},
			}
			if a.Name == ActionToken {
				cases = append(cases, struct {
					name   string
					attach database.Attachment
					status string
					code   string
					want   int
				}{"tokens off", database.Attachment{Connected: true, Attached: true, Enabled: true},
					"", k.Name + "_tokens_off", http.StatusForbidden})
			}
			for _, tc := range cases {
				t.Run(k.Name+"/"+a.Name+"/"+tc.name, func(t *testing.T) {
					store := &fakeStore{attach: map[string]database.Attachment{k.Name: tc.attach}}
					d, err := Check(store, "app1", tc.status, capability)
					if err != nil {
						t.Fatalf("Check: %v", err)
					}
					if d.Allowed {
						t.Fatalf("allowed, want %q", tc.code)
					}
					if d.Code != tc.code {
						t.Errorf("code = %q, want %q", d.Code, tc.code)
					}
					if d.Status != tc.want {
						t.Errorf("status = %d, want %d", d.Status, tc.want)
					}
					if strings.TrimSpace(d.Reason) == "" {
						t.Error("refusal with no words")
					}
				})
			}

			t.Run(k.Name+"/"+a.Name+"/allowed", func(t *testing.T) {
				store := &fakeStore{attach: map[string]database.Attachment{k.Name: open}}
				d, err := Check(store, "app1", database.AppStatusRunning, capability)
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				if !d.Allowed {
					t.Errorf("refused an open attachment: %s", d.Reason)
				}
			})
		}
	}
}

// Only the three file kinds issue tokens, each says what to do when its
// connection's switch is off, and a file kind's first file-carrying action is
// still the upload that /deliver resolves.
func TestOnlyTheFileKindsIssueTokens(t *testing.T) {
	want := map[string]bool{IntegrationSharePoint: true, IntegrationOneDrive: true,
		IntegrationGoogleDrive: true}
	for _, k := range Known {
		if k.IssuesTokens() != want[k.Name] {
			t.Errorf("%s issues tokens = %v, want %v", k.Name, k.IssuesTokens(), want[k.Name])
		}
		if (k.Refusals.TokensOff != "") != k.IssuesTokens() {
			t.Errorf("%s: a TokensOff sentence exactly when the kind issues tokens", k.Name)
		}
		if k.IssuesTokens() {
			if f := k.FileAction(); f == nil || f.Name != "upload" || k.Actions[0].Name != "upload" {
				t.Errorf("%s: the upload is no longer the kind's first file action", k.Name)
			}
		}
	}
}

// A kind nobody registered is a 400 that says so, not a 403 and not a 404: the
// caller named something that does not exist rather than something they may not
// see.
func TestAnUnknownKindIsABadRequest(t *testing.T) {
	for _, capability := range []string{"carrier.pigeon", "slack.telepathy", "", "slack"} {
		d, err := Check(bothOn(), "app1", database.AppStatusRunning, capability)
		if err != nil {
			t.Fatalf("Check(%q): %v", capability, err)
		}
		if d.Allowed {
			t.Fatalf("%q was allowed", capability)
		}
		if d.Code != "unknown_capability" || d.Status != http.StatusBadRequest {
			t.Errorf("%q → code %q status %d, want unknown_capability/400",
				capability, d.Code, d.Status)
		}
	}
}

// A name an app supplies for a file is a *name*, never a path — and one that
// tries to be a path is refused rather than sanitised.
//
// The refusal is the point. A sanitiser that quietly rewrites `reports/june.csv`
// to `june.csv` hands the app a file it cannot find again, in a folder it cannot
// list, under a name it did not choose, and gives it no way to learn that
// happened.
func TestFilenameSeparatorsRefused(t *testing.T) {
	for _, name := range []string{
		"reports/june.csv",
		`reports\june.csv`,
		"../june.csv",
		"..",
		".hidden",
		"CON",
		"con.txt",
		"lpt1.csv",
		"june\x00.csv",
		"",
		"   ",
	} {
		if err := CheckFilename(name); err == nil {
			t.Errorf("%q was accepted as a filename", name)
		}
	}
	for _, name := range []string{
		"june.csv",
		"June Report (final).xlsx",
		"report-2026-W32.json",
		"contains.dots.txt",
		"console.log",
	} {
		if err := CheckFilename(name); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}

// The settings index has to be able to find every kind.
//
// The catalog is Go and the index is TypeScript, so nothing but a test can hold
// them together — the same arrangement `secrets_registry_test.go` has with
// `schema.sql`, and for the same reason: the alternative is a person searching
// ⌘K for "sharepoint", finding nothing, and concluding this install does not
// have it.
//
// **It checks the keywords rather than demanding a row per kind**, and that is
// a deliberate departure from what the PRD asked for. A connection is a row an
// administrator creates, not a value they set: `settings-index.test.ts` would
// then require a rendered control id per kind, and there is none — the page
// renders one form per *connection*, whose ids are per row. The index carries
// two composite entries for the page, exactly as `git_connections` does for
// Sources, and this is the guard that keeps them naming everything.
func TestEverySettingsIndexEntryCanFindEveryKind(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "settings-index.ts"))
	if err != nil {
		t.Skipf("the settings index is not readable from here: %v", err)
	}
	index := strings.ToLower(string(source))
	for _, k := range Known {
		// Mail's own settings live on the Email page, and the index has covered
		// them since before this catalog existed.
		if k.Name == IntegrationEmail {
			continue
		}
		if !strings.Contains(index, k.Name) {
			t.Errorf("nothing in the settings index mentions %q — somebody searching for it "+
				"will conclude this install does not have it", k.Name)
		}
	}
}

// **A list on the wire is a list, never `null`.**
//
// A nil slice marshals to `null`, and every consumer then has to guard — which
// is exactly what the admin page did not do, and the first expanded connection
// crashed the whole page with "Cannot read properties of null". A guard per
// consumer is a rule nobody can enforce; a non-nil slice per entry is one this
// test can.
func TestNoKindEmitsANullList(t *testing.T) {
	blob, err := json.Marshal(Known)
	if err != nil {
		t.Fatalf("marshalling the catalog: %v", err)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("unmarshalling the catalog: %v", err)
	}
	lists := []string{"credentials", "actions", "platform_audiences"}
	for i, entry := range raw {
		for _, field := range lists {
			if string(entry[field]) == "null" {
				t.Errorf("%s.%s is null on the wire — every reader would have to guard",
					Known[i].Name, field)
			}
		}
	}
}

// The hint for an unknown capability spells the shapes the catalog has — a
// send's integration.action and a file integration's .token — so an author
// asking for SharePoint is not left guessing from "like slack.post" (D-15).
func TestTheUnknownCapabilityHintNamesTheRealShapes(t *testing.T) {
	d, _ := Check(bothOn(), "app1", database.AppStatusRunning, "sharepoint.access_token")
	for _, want := range []string{"GET /api/v1/app/self", "sharepoint.upload",
		"sharepoint.token", "onedrive.token", "gdrive.token"} {
		if !strings.Contains(d.Hint, want) {
			t.Errorf("hint %q does not name %s", d.Hint, want)
		}
	}
}
