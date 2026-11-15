package sdk

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shared TOFT_STORAGE and respond vectors (testdata/), R's half. Go, Node
// and Python each read the files from their own suites; R's suite is this one,
// so the R script reads the same files and answers one JSON line per case,
// which the expectations are held against here.
func TestTheRSDKReadsTheSharedVectors(t *testing.T) {
	if _, err := exec.LookPath("Rscript"); err != nil {
		t.Skip("no Rscript on PATH")
	}
	if !rHas("curl") || !rHas("jsonlite") || !rHas("openssl") {
		t.Skip("this R lacks curl, jsonlite or openssl, which the R SDK imports")
	}

	storageVector, _ := filepath.Abs(filepath.Join("testdata", "storage-env-vector.json"))
	respondVector, _ := filepath.Abs(filepath.Join("testdata", "respond-vector.json"))
	script := `
for (f in list.files("R", full.names = TRUE)) source(f)
args <- commandArgs(trailingOnly = TRUE)
emit <- function(x) cat(as.character(jsonlite::toJSON(x, auto_unbox = TRUE, null = "null")), "\n", sep = "")
refusal_of <- function(e) list(error = e$code, hinted = !is.null(e$body$hint) && nzchar(e$body$hint))

for (tc in jsonlite::fromJSON(args[1], simplifyVector = FALSE)$cases) {
  if (is.null(tc$raw)) Sys.unsetenv("TOFT_STORAGE") else Sys.setenv(TOFT_STORAGE = tc$raw)
  got <- tryCatch(list(mounts = mounts()), toft_error = refusal_of,
                  error = function(e) list(error = paste("raw:", conditionMessage(e))))
  if (!is.null(got$mounts) && length(got$mounts) == 0) got$mounts <- I(list())
  emit(c(list(vector = "storage", case = tc$name), got))
}

for (tc in jsonlite::fromJSON(args[2], simplifyVector = FALSE)$cases) {
  run <- if (!is.null(tc$string)) tc$string else as_run(tc$run)
  for (accept in c("application/json", "text/html")) {
    response <- new.env()
    response$headers <- list()
    response$setHeader <- function(name, value) response$headers[[name]] <- value
    got <- tryCatch({
      out <- job_respond(run, res = response, req = list(HEADERS = c(accept = accept)))
      list(status = out$status, location = out$headers[["Location"]])
    }, toft_error = refusal_of, error = function(e) list(error = paste("raw:", conditionMessage(e))))
    got$written <- !is.null(response$status) || length(response$headers) > 0
    emit(c(list(vector = "respond", case = tc$name, accept = accept), got))
  }
}
`
	cmd := exec.Command("Rscript", "-e", script, storageVector, respondVector)
	cmd.Dir = "r"
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TOFT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rscript: %v\n%s", err, out)
	}

	type answer struct {
		Vector   string            `json:"vector"`
		Case     string            `json:"case"`
		Accept   string            `json:"accept"`
		Error    string            `json:"error"`
		Hinted   bool              `json:"hinted"`
		Mounts   []json.RawMessage `json:"mounts"`
		Status   int               `json:"status"`
		Location string            `json:"location"`
		Written  bool              `json:"written"`
	}
	answers := map[string]answer{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var a answer
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("R answered a line that is not JSON: %q\n%s", line, out)
		}
		answers[a.Vector+"/"+a.Case+"/"+a.Accept] = a
	}

	var storage struct {
		Cases []struct {
			Name   string           `json:"name"`
			Mounts []map[string]any `json:"mounts"`
			Error  string           `json:"error"`
		} `json:"cases"`
	}
	readVector(t, storageVector, &storage)
	for _, tc := range storage.Cases {
		got, ok := answers["storage/"+tc.Name+"/"]
		switch {
		case !ok:
			t.Errorf("storage %q: R gave no answer", tc.Name)
		case tc.Error != "":
			if got.Error != tc.Error || !got.Hinted {
				t.Errorf("storage %q: R answered %+v, want a hinted %s", tc.Name, got, tc.Error)
			}
		case got.Error != "":
			t.Errorf("storage %q: R refused %s", tc.Name, got.Error)
		default:
			var decoded []map[string]any
			for _, m := range got.Mounts {
				var entry map[string]any
				json.Unmarshal(m, &entry)
				decoded = append(decoded, entry)
			}
			a, _ := json.Marshal(decoded)
			b, _ := json.Marshal(tc.Mounts)
			if len(decoded) == 0 {
				a = []byte("[]")
			}
			if string(a) != string(b) {
				t.Errorf("storage %q: R's mounts() =\n %s\nwant\n %s", tc.Name, a, b)
			}
		}
	}

	type respondAnswer struct {
		Status   int    `json:"status"`
		Location string `json:"location"`
	}
	var respond struct {
		Cases []struct {
			Name  string         `json:"name"`
			JSON  *respondAnswer `json:"json"`
			HTML  *respondAnswer `json:"html"`
			Error string         `json:"error"`
		} `json:"cases"`
	}
	readVector(t, respondVector, &respond)
	for _, tc := range respond.Cases {
		for accept, want := range map[string]*respondAnswer{"application/json": tc.JSON, "text/html": tc.HTML} {
			got, ok := answers["respond/"+tc.Name+"/"+accept]
			switch {
			case !ok:
				t.Errorf("respond %q (%s): R gave no answer", tc.Name, accept)
			case tc.Error != "":
				if got.Error != tc.Error || !got.Hinted || got.Written {
					t.Errorf("respond %q (%s): R answered %+v, want a hinted %s with nothing written", tc.Name, accept, got, tc.Error)
				}
			case got.Error != "" || got.Status != want.Status || got.Location != want.Location:
				t.Errorf("respond %q (%s): R answered %+v, want %d %q", tc.Name, accept, got, want.Status, want.Location)
			}
		}
	}
}

func readVector(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}
