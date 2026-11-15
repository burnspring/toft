package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/runtime"
)

// storageDrive is one declared place volumes live — a host root or a cluster
// storage class — as Admin → Storage lists it (ADR §12a). The key is the
// operator's; the label is the administrator's.
type storageDrive struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"` // "root" or "class"
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Path is a root's directory on this host; Class a cluster drive's class.
	Path    string `json:"path,omitempty"`
	Class   string `json:"class,omitempty"`
	Backing string `json:"backing,omitempty"`
	// ReadOnly is a `:ro` root.
	ReadOnly bool `json:"read_only,omitempty"`
	// Default is a class named by KUBE_STORAGE_CLASS or KUBE_S3FILES_STORAGE_CLASS:
	// where a volume naming no class lives.
	Default bool `json:"default,omitempty"`
	// Declared is false for a name resources still record that the install no
	// longer declares — a stranded drive, which a re-point moves them off.
	Declared      bool `json:"declared"`
	ResourceCount int  `json:"resource_count"`
	// DeleteRemovesData is whether deleting a resource on this drive removes
	// its files — a writable host root always, a class when its driver is known
	// to (kube.deleteVerdict). The delete dialog asks for the name exactly when
	// this is true, and says what survives when it is not.
	DeleteRemovesData bool `json:"delete_removes_data"`
}

const maxDriveLabelLen = 60

// effectiveClass is the class a cluster volume is bound from: its own, or the
// declared default for its backing. Empty on a host install.
func effectiveClass(res database.StorageResource, classes []runtime.DeclaredClass) string {
	if res.Kind != database.StorageKindVolume || res.Root != "" {
		return ""
	}
	if res.StorageClass != "" {
		return res.StorageClass
	}
	for _, c := range classes {
		if c.Default && c.Serves(res.Backing) {
			return c.Name
		}
	}
	return ""
}

// storageDrives assembles every drive: the declared ones in declaration order
// (roots by name, classes defaults-first), then any name resources still record
// that nothing declares. Labels are read once.
func (s *Server) storageDrives(resources []database.StorageResource) ([]storageDrive, error) {
	labels, err := s.db.ListStorageDriveLabels()
	if err != nil {
		return nil, err
	}
	roots := s.storageRoots()
	classes := s.declaredClasses()

	counts := map[string]int{}
	for _, r := range resources {
		switch {
		case r.Root != "":
			counts[database.DriveKeyForRoot(r.Root)]++
		case effectiveClass(r, classes) != "":
			counts[database.DriveKeyForClass(effectiveClass(r, classes))]++
		}
	}

	out := []storageDrive{}
	seen := map[string]bool{}
	add := func(d storageDrive) {
		d.Label = d.Name
		if l, ok := labels[d.Key]; ok {
			d.Label, d.Description = l.Label, l.Description
		}
		d.ResourceCount = counts[d.Key]
		seen[d.Key] = true
		out = append(out, d)
	}
	for _, r := range roots {
		add(storageDrive{Key: database.DriveKeyForRoot(r.Name), Kind: "root", Name: r.Name,
			Path: r.Path, ReadOnly: r.ReadOnly, Declared: true, DeleteRemovesData: !r.ReadOnly})
	}
	for _, c := range classes {
		add(storageDrive{Key: database.DriveKeyForClass(c.Name), Kind: "class", Name: c.Name,
			Class: c.Name, Backing: c.Backing, Default: c.Default, Declared: true,
			DeleteRemovesData: c.DeleteRemovesData})
	}

	var stranded []string
	for key := range counts {
		if !seen[key] {
			stranded = append(stranded, key)
		}
	}
	sort.Strings(stranded)
	for _, key := range stranded {
		kind, name, _ := strings.Cut(key, ":")
		d := storageDrive{Key: key, Kind: kind, Name: name}
		if kind == "class" {
			d.Class = name
		}
		add(d)
	}
	return out, nil
}

type driveLabelRequest struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// handlePutStorageDriveLabel — PATCH /admin/storage/drives/{key}. Sets what a
// drive is called; an empty label clears it back to the drive's name. A label
// is for people only, so nothing restarts and nothing is re-provisioned.
func (s *Server) handlePutStorageDriveLabel(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	if s.refusedUnlicensed(w, r, entitlements.FeatureAppStorage) {
		return
	}
	// Unescaped, because a client that encodes the key sends `root%3Aoms` and
	// chi hands back the raw segment when the path carried an escape.
	key, err := url.PathUnescape(chi.URLParam(r, "key"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid drive key", "hint": "GET /api/v1/admin/storage lists the drives with their keys"})
		return
	}

	var req driveLabelRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	label := strings.TrimSpace(req.Label)
	description := strings.TrimSpace(req.Description)
	switch {
	case strings.ContainsAny(label, "\r\n"):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "a label is one line", "hint": "send the label on a single line, without line breaks"})
		return
	case utf8.RuneCountInString(label) > maxDriveLabelLen:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("a label is at most %d characters", maxDriveLabelLen), "hint": fmt.Sprintf("shorten label to %d characters or fewer", maxDriveLabelLen)})
		return
	case len(description) > maxStorageDescriptionLen:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("a description is at most %d characters", maxStorageDescriptionLen), "hint": fmt.Sprintf("shorten description to %d characters or fewer", maxStorageDescriptionLen)})
		return
	}

	resources, err := s.db.ListStorageResources()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	drives, err := s.storageDrives(resources)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	var drive *storageDrive
	for i := range drives {
		if drives[i].Key == key {
			drive = &drives[i]
		}
	}
	// 404 for a key nothing declares and nothing records: a label on it would
	// label nothing.
	if drive == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage lists the drives with their keys"})
		return
	}
	// One select tells drives apart by what it prints, so a label may equal no
	// other drive's label and no other drive's name (D3).
	if label != "" {
		for _, d := range drives {
			if d.Key == key {
				continue
			}
			if strings.EqualFold(d.Label, label) || strings.EqualFold(d.Name, label) {
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": fmt.Sprintf("%q is already what drive %s is called", label, d.Name), "hint": "choose a label no other drive uses — GET /api/v1/admin/storage lists the drives with their labels"})
				return
			}
		}
	}
	switch {
	case label == drive.Name && description == "":
		label = "" // labelling a drive with its own name is no label at all
	case label == "" && description != "":
		label = drive.Name // a description needs a row, and the row a label
	}

	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())
	if err := s.db.PutStorageDriveLabel(key, label, description, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "another drive already has that label", "hint": "choose a label no other drive uses — GET /api/v1/admin/storage lists the drives with their labels"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	drives, err = s.storageDrives(resources)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	for _, d := range drives {
		if d.Key == key {
			writeJSON(w, http.StatusOK, d)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

type repointRequest struct {
	// To is the drive the resources now live on: a key (`root:oms`) or a bare
	// name, read as the same kind as the drive being re-pointed.
	To     string `json:"to"`
	DryRun bool   `json:"dry_run"`
}

// repointCheck is one resource's evidence: the directory found under the new
// root, or the file system its bound volumes are on.
type repointCheck struct {
	Slug     string `json:"slug"`
	Evidence string `json:"evidence,omitempty"`
	Problem  string `json:"problem,omitempty"`
}

type repointAnswer struct {
	From   string         `json:"from"`
	To     string         `json:"to"`
	Checks []repointCheck `json:"checks"`
	Moved  bool           `json:"moved"`
}

// handleRepointStorageDrive — POST /admin/storage/drives/{key}/repoint.
//
// Moves every resource recorded against a drive this install no longer
// declares onto the declared drive that now holds their data — **all of them
// or none, and only after proving the data is there** (PRD D8–D11). The proof
// is per shape: a directory per resource under the new root, or the resource's
// own bound volumes on the new class's file system. `dry_run` answers the proof
// without writing, which is what the confirm dialog shows; the real call proves
// again rather than trusting an earlier answer. Nothing restarts: each app reads
// its new path at its next start.
func (s *Server) handleRepointStorageDrive(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	if s.refusedUnlicensed(w, r, entitlements.FeatureAppStorage) {
		return
	}
	key, err := url.PathUnescape(chi.URLParam(r, "key"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid drive key", "hint": "GET /api/v1/admin/storage lists the drives with their keys"})
		return
	}
	var req repointRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}

	resources, err := s.db.ListStorageResources()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	drives, err := s.storageDrives(resources)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	find := func(k string) *storageDrive {
		for i := range drives {
			if drives[i].Key == k {
				return &drives[i]
			}
		}
		return nil
	}

	from := find(key)
	switch {
	case from == nil:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage lists the drives with their keys"})
		return
	case from.Declared:
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf(
			"%s is declared, so its resources aren't stranded. A re-point moves resources off a drive "+
				"this install no longer declares.", from.Name), "hint": "leave this drive as it is — re-point only a drive GET /api/v1/admin/storage shows as no longer declared"})
		return
	}
	toKey := strings.TrimSpace(req.To)
	if !strings.Contains(toKey, ":") {
		toKey = from.Kind + ":" + toKey
	}
	to := find(toKey)
	switch {
	case to == nil || !to.Declared:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "re-point onto a drive this install declares", "hint": `name in "to" a drive GET /api/v1/admin/storage shows as declared`})
		return
	case to.Kind != from.Kind:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "a host drive's resources re-point onto a host drive, and a cluster drive's onto a cluster drive", "hint": `name in "to" a declared drive of the same kind as the one being re-pointed`})
		return
	}

	classes := s.declaredClasses()
	var on []database.StorageResource
	for _, res := range resources {
		if (from.Kind == "root" && res.Root == from.Name) ||
			(from.Kind == "class" && effectiveClass(res, classes) == from.Name) {
			on = append(on, res)
		}
	}

	var checks []repointCheck
	if from.Kind == "root" {
		checks = s.proveOnRoot(on, *to)
	} else {
		var refusal string
		checks, refusal = s.proveOnClass(r.Context(), on, *to)
		if refusal != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": refusal, "hint": "fix what the check reports on the target drive, then re-point again"})
			return
		}
	}

	answer := repointAnswer{From: from.Key, To: to.Key, Checks: checks}
	var problems []string
	for _, c := range checks {
		if c.Problem != "" {
			problems = append(problems, c.Slug+": "+c.Problem)
		}
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "nothing was moved: " + strings.Join(problems, "; "),
			"checks": checks, "from": from.Key, "to": to.Key, "hint": "resolve each problem listed, then re-point again — nothing was changed"})
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, answer)
		return
	}

	slugs := make([]string, len(on))
	for i, res := range on {
		slugs[i] = res.Slug
	}
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())
	if err := s.db.RepointStorageResources(from.Kind, from.Name, to.Name, slugs,
		p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if errors.Is(err, database.ErrRepointRaced) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "reload the drives with GET /api/v1/admin/storage and try the re-point again"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	answer.Moved = true
	writeJSON(w, http.StatusOK, answer)
}

// proveOnRoot is the host proof (D9): every resource's directory exists under
// the new root, and the new root's mount serves every attachment's level — in
// shared mode a level is which mount a resource is under, so a resource mapped
// to write cannot land on a `:ro` root, nor a read-only one on a writable root.
func (s *Server) proveOnRoot(on []database.StorageResource, to storageDrive) []repointCheck {
	checks := make([]repointCheck, 0, len(on))
	for _, res := range on {
		c := repointCheck{Slug: res.Slug}
		dir := filepath.Join(to.Path, res.Slug)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			c.Problem = "no directory at " + dir
			checks = append(checks, c)
			continue
		}
		c.Evidence = dir
		mappings, err := s.db.ListStorageMappingsForResource(res.ID)
		if err != nil {
			c.Problem = "its attachments could not be read"
		}
		for _, m := range mappings {
			readOnly := m.Access == runtime.AccessRead
			if readOnly != to.ReadOnly {
				if to.ReadOnly {
					c.Problem = fmt.Sprintf("%s is attached at %s, and %s is read-only", m.AppName, m.Access, to.Name)
				} else {
					c.Problem = fmt.Sprintf("%s is attached read-only, and %s is read-write", m.AppName, to.Name)
				}
				break
			}
		}
		checks = append(checks, c)
	}
	return checks
}

// classProver is the cluster backend's half of a re-point's proof.
type classProver interface {
	ClassFileSystem(ctx context.Context, class string) (provisioner, fileSystem string, err error)
	BoundFileSystems(ctx context.Context, res runtime.StorageResource, folders []string) ([]string, error)
}

// proveOnClass is the cluster proof (D10): every resource's bound volumes are on
// the file system the new class provisions on. A resource never bound carries
// no evidence and is refused; so is a class that cannot be read, or that names
// no single file system — Azure Files makes a share per claim, so "the same
// file system under a new name" means nothing there. The second return is a
// refusal of the whole request, before any resource is looked at.
func (s *Server) proveOnClass(ctx context.Context, on []database.StorageResource, to storageDrive) ([]repointCheck, string) {
	prover, ok := s.storage.(classProver)
	if !ok {
		return nil, "this install's storage backend cannot read where a cluster drive's volumes are bound"
	}
	if to.Backing == runtime.BackingAzureFiles {
		return nil, "re-pointing needs a single file system, and Azure Files makes a share per claim"
	}
	provisioner, fileSystem, err := prover.ClassFileSystem(ctx, to.Class)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("storage class %s could not be read, so nothing proves where it provisions: %v", to.Class, err)
	case provisioner != "efs.csi.aws.com":
		return nil, fmt.Sprintf("storage class %s uses %s; re-pointing is proven for EFS and S3 Files classes only", to.Class, provisioner)
	case fileSystem == "":
		return nil, fmt.Sprintf("storage class %s names no fileSystemId, so nothing says which file system it is", to.Class)
	}

	checks := make([]repointCheck, 0, len(on))
	for _, res := range on {
		c := repointCheck{Slug: res.Slug}
		var folders []string
		if mappings, err := s.db.ListStorageMappingsForResource(res.ID); err == nil {
			for _, m := range mappings {
				folders = append(folders, m.Folder)
			}
		}
		bound, err := prover.BoundFileSystems(ctx, storageResourceFor(&res, "", ""), folders)
		switch {
		case err != nil:
			c.Problem = err.Error()
		case len(bound) == 0:
			c.Problem = "it has never been bound, so nothing proves it lives on " + fileSystem
		default:
			for _, fs := range bound {
				if fs != fileSystem {
					c.Problem = fmt.Sprintf("its volumes are on %s, not %s", fs, fileSystem)
					break
				}
			}
			if c.Problem == "" {
				c.Evidence = "bound to " + fileSystem
			}
		}
		checks = append(checks, c)
	}
	return checks, ""
}
