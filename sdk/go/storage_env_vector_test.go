package toft

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The shared TOFT_STORAGE vector, read by all four SDKs: sdk/testdata is the
// spec, and this is Go's half of it. The file lives one directory up because
// sdk/go is the shipped copy and carries no fixtures of its own.
type storageEnvCase struct {
	Name   string           `json:"name"`
	Raw    *string          `json:"raw"`
	Mounts []map[string]any `json:"mounts"`
	Error  string           `json:"error"`
}

func TestMountsReadsTheSharedStorageEnvVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "storage-env-vector.json"))
	if err != nil {
		t.Fatalf("reading the storage vector: %v", err)
	}
	var vector struct {
		Cases []storageEnvCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parsing the storage vector: %v", err)
	}
	if len(vector.Cases) == 0 {
		t.Fatal("the storage vector has no cases")
	}
	for _, tc := range vector.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if tc.Raw == nil {
				t.Setenv(EnvStorage, "")
				os.Unsetenv(EnvStorage)
			} else {
				t.Setenv(EnvStorage, *tc.Raw)
			}
			got, err := Mounts()
			if tc.Error != "" {
				var e *Error
				if !errors.As(err, &e) {
					t.Fatalf("Mounts() = %v, %v; want %s", got, err, tc.Error)
				}
				if e.Code() != tc.Error || e.Hint() == "" {
					t.Errorf("Mounts() refused %q with hint %q, want %s with a hint", e.Code(), e.Hint(), tc.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("Mounts() = %v", err)
			}
			encoded, _ := json.Marshal(got)
			var decoded []map[string]any
			json.Unmarshal(encoded, &decoded)
			want, _ := json.Marshal(tc.Mounts)
			again, _ := json.Marshal(decoded)
			if string(again) != string(want) {
				t.Errorf("Mounts() =\n %s\nwant\n %s", again, want)
			}
		})
	}
}

// The shared respond vector. Go's Respond takes a typed JobRun, so the case a
// bare string stands for cannot be written here and is skipped.
func TestRespondReadsTheSharedRespondVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "respond-vector.json"))
	if err != nil {
		t.Fatalf("reading the respond vector: %v", err)
	}
	type answer struct {
		Status   int    `json:"status"`
		Location string `json:"location"`
	}
	var vector struct {
		Cases []struct {
			Name   string          `json:"name"`
			Run    json.RawMessage `json:"run"`
			String *string         `json:"string"`
			JSON   *answer         `json:"json"`
			HTML   *answer         `json:"html"`
			Error  string          `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatalf("parsing the respond vector: %v", err)
	}
	for _, tc := range vector.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if tc.String != nil {
				t.Skip("Respond takes a JobRun; the compiler refuses a string")
			}
			var run JobRun
			if err := json.Unmarshal(tc.Run, &run); err != nil {
				t.Fatalf("decoding the run: %v", err)
			}
			for accept, want := range map[string]*answer{"application/json": tc.JSON, "text/html": tc.HTML} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/x", nil)
				r.Header.Set("Accept", accept)
				err := Jobs.Respond(w, r, run)
				if tc.Error != "" {
					var e *Error
					if !errors.As(err, &e) || e.Code() != tc.Error || e.Hint() == "" {
						t.Errorf("Accept %s: Respond = %v, want a hinted %s", accept, err, tc.Error)
					}
					if w.Header().Get("Location") != "" || w.Body.Len() != 0 || len(w.Header()) != 0 {
						t.Errorf("Accept %s: a refused Respond wrote %v %q", accept, w.Header(), w.Body.String())
					}
					continue
				}
				if err != nil {
					t.Fatalf("Accept %s: Respond = %v", accept, err)
				}
				if w.Code != want.Status || w.Header().Get("Location") != want.Location {
					t.Errorf("Accept %s: %d Location %q, want %d %q", accept, w.Code, w.Header().Get("Location"), want.Status, want.Location)
				}
			}
		})
	}
}
