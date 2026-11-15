package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
	"toft/internal/runner"
)

func TestRefuseEnvNames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		vars   map[string]string
		stored map[string]database.EnvVar
		code   string
		field  string
	}{
		{"fine", map[string]string{"DATABASE_URL": "x", "PATH": "/opt/bin"}, nil, "", ""},
		{"invalid", map[string]string{"OK": "1", "MY-VAR": "x"}, nil, runner.EnvNameInvalidCode, "MY-VAR"},
		{"reserved", map[string]string{"PORT": "3000"}, nil, runner.EnvNameReservedCode, "PORT"},
		// A name saved before the check is resubmitted by every save of the
		// form; refusing it would refuse every later edit of the app.
		{"stored before the check", map[string]string{"PORT": "3000", "A": "b"},
			map[string]database.EnvVar{"PORT": {Key: "PORT"}}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			refused := refuseEnvNames(w, tc.vars, tc.stored)
			if refused != (tc.code != "") {
				t.Fatalf("refused = %v, want %v", refused, tc.code != "")
			}
			if !refused {
				return
			}
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d", w.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] != tc.code || body["name"] != tc.field || body["error"] == "" {
				t.Errorf("body = %v", body)
			}
		})
	}
}
