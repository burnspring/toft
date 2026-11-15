package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strings"

	"toft/internal/database"
)

// storageResourceOr404 loads the resource a route's {id} names, or answers why
// not. **The refusal says which thing was missing and, when the caller sent a
// name, the id to send instead.** A bare "not found" from a route that names
// both a resource and an app sent an agent to probe the body, the app and the
// path in turn, when the path held a resource's name where its id belonged —
// the one mistake every caller who reads `toft storage ls` makes first.
func (s *Server) storageResourceOr404(w http.ResponseWriter, id string) (*database.StorageResource, bool) {
	res, err := s.db.GetStorageResource(id)
	if err != nil {
		log.Printf("loading storage %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	if res != nil {
		return res, true
	}
	msg := fmt.Sprintf("no storage resource has the id %q — GET /api/v1/admin/storage lists them", id)
	if byName, _, err := s.db.StorageResourceByName(id); err == nil && byName != nil {
		msg = fmt.Sprintf("%q is a storage resource's name, and this route takes its id: %s", id, byName.ID)
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": msg, "code": "storage_not_found", "hint": "GET /api/v1/admin/storage lists the storage resources with their ids"})
	return nil, false
}

// decodeStrictBody decodes a request body that must hold only the fields v
// declares, or answers 400 naming the field it did not know and the ones it
// does. **A field the route ignores is a request that did something other than
// what was sent** — `{"mount":"/data"}` attaching at the default path — and
// the caller learns that only from what happens at the next deploy.
func decodeStrictBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		return true
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%s is not a field this route takes; it takes %s",
				field, strings.Join(jsonFieldNames(v), ", ")),
			"code": "unknown_field", "hint": "drop that field and send only the ones this route takes"})
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": fmt.Sprintf("the body is not the JSON object this route takes (%s): %v",
			strings.Join(jsonFieldNames(v), ", "), err),
		"code": "invalid_json", "hint": badBodyHint})
	return false
}

// jsonFieldNames is the json names of a struct's fields, in order.
func jsonFieldNames(v any) []string {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}
