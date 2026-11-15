package builder

import (
	"context"
	"fmt"
	"strings"

	"toft/internal/database"
	"toft/internal/runtime"
)

// toft.toml's `[[storage]]` — the storage an app needs, declared in its repo
// (ADR § Storage somebody else can attach).
//
//	[[storage]]
//	name   = "sales-data"
//	access = "read"          # default
//	folder = "2026"          # optional
//	path   = "/data/sales"   # default /storage/<name>[/<folder>]
//
// **An administrator still creates every resource and every folder**; the
// manifest only attaches what exists. Each entry is held by the app *owner's*
// delegation and stamped to it, and the deployer is checked on changes only.
//
// **The table is the serving release's declaration.** A deploy plans after the
// clone, starts its workload from the plan, and writes the table at the flip
// and nowhere else — so a crash restart, a job run or a signed-URL mint during
// the build still reads the release that is serving.

// manifestStorage is one entry as the decoder reads it.
type manifestStorage struct {
	Name   string `toml:"name"`
	Access string `toml:"access"`
	Folder string `toml:"folder"`
	Path   string `toml:"path"`
}

// StorageEntry is one `[[storage]]` entry, with its defaults filled in.
type StorageEntry struct {
	Name   string `json:"name"`
	Access string `json:"access"`
	Folder string `json:"folder,omitempty"`
	Path   string `json:"path"`
}

// DefaultStoragePath is where an entry with no `path` is mounted. Code reads
// TOFT_STORAGE by name, so the path is a default rather than a requirement.
func DefaultStoragePath(name, folder string) string {
	if folder == "" {
		return "/storage/" + name
	}
	return "/storage/" + name + "/" + folder
}

// parseManifestStorage judges the entries on their own: what the file can be
// refused for without knowing the install. Rights, ladders and folders are the
// planner's, through the console's own validator.
func parseManifestStorage(raw []manifestStorage) ([]StorageEntry, error) {
	var errs []error
	out := make([]StorageEntry, 0, len(raw))
	byKey := map[string]int{}
	byPath := map[string]int{}
entries:
	for i, r := range raw {
		field := func(key string) string { return fmt.Sprintf("storage[%d].%s", i, key) }
		e := StorageEntry{
			Name:   strings.TrimSpace(r.Name),
			Access: strings.TrimSpace(r.Access),
			Folder: strings.TrimSuffix(strings.TrimSpace(r.Folder), "/"),
			Path:   strings.TrimSpace(r.Path),
		}
		// The name and the access are judged apart, so an entry wrong in both
		// says both; the path and the duplicates need a name to mean anything.
		nameBad := true
		if e.Name == "" {
			errs = append(errs, ManifestError{Field: field("name"), Code: StorageEntryInvalid, Msg: "is required: the name of a storage resource on this install"})
		} else if err := database.ValidateStorageName(e.Name); err != nil {
			errs = append(errs, ManifestError{Field: field("name"), Code: StorageEntryInvalid, Msg: err.Error()})
		} else {
			nameBad = false
		}
		switch e.Access {
		case "":
			// Left empty: the level is the planner's to choose, because on a
			// shared install it is the root's (validateAttach), and elsewhere
			// read.
		case runtime.AccessRead, runtime.AccessWrite, runtime.AccessFull:
		default:
			errs = append(errs, ManifestError{Field: field("access"), Code: StorageEntryInvalid,
				Msg: fmt.Sprintf("is read, write or full, not %q", e.Access)})
			continue entries
		}
		if nameBad {
			continue entries
		}
		if e.Path == "" {
			e.Path = DefaultStoragePath(e.Name, e.Folder)
		}
		if err := database.ValidateMountPath(e.Path); err != nil {
			errs = append(errs, ManifestError{Field: field("path"), Code: StorageEntryInvalid, Msg: err.Error()})
			continue entries
		}
		key := e.Name + "\x00" + e.Folder
		if j, dup := byKey[key]; dup {
			what := fmt.Sprintf("%q", e.Name)
			if e.Folder != "" {
				what = fmt.Sprintf("folder %q of %q", e.Folder, e.Name)
			}
			errs = append(errs, ManifestError{Field: field("name"), Code: StorageNameAmbiguous,
				Msg: fmt.Sprintf("declares %s twice (storage[%d] too) — an app mounts each "+
					"resource or folder once; give each mount a folder of its own", what, j)})
			continue entries
		}
		if j, dup := byPath[e.Path]; dup {
			errs = append(errs, ManifestError{Field: field("path"), Code: StoragePathTaken,
				Msg: fmt.Sprintf("mounts at %s, which storage[%d] mounts at too", e.Path, j)})
			continue entries
		}
		for p, j := range byPath {
			if database.MountPathsNest(e.Path, p) {
				errs = append(errs, ManifestError{Field: field("path"), Code: StoragePathTaken,
					Msg: fmt.Sprintf("mounts at %s, which nests with storage[%d]'s %s — one mount "+
						"cannot sit inside another; map the folder on its own, or the whole resource, not both", e.Path, j, p)})
				continue entries
			}
		}
		byKey[key], byPath[e.Path] = i, i
		out = append(out, e)
	}
	return out, joinManifestErrors(errs)
}

// The reason codes a storage refusal carries in `toft check` and in a deploy's
// failure (PRD §5). The ones the console's validator also makes are its codes,
// in internal/api.
const (
	// StorageEntryInvalid is an entry the file alone refuses — no name, an
	// access that is not read/write/full, a mount path the runner owns.
	StorageEntryInvalid            = "storage_entry_invalid"
	StorageNameAmbiguous           = "storage_name_ambiguous"
	StorageUnknown                 = "storage_unknown"
	StorageNotAttachableByOwner    = "storage_not_attachable_by_owner"
	StorageNotAttachableByYou      = "storage_not_attachable_by_you"
	StorageLevelAboveCeiling       = "storage_level_above_ceiling"
	StorageRequiresPerson          = "storage_requires_person"
	StorageConflictsConsole        = "storage_conflicts_console_mapping"
	StoragePathTaken               = "storage_path_taken"
	StorageDeleting                = "storage_deleting"
	StorageNeedsRoot               = "storage_needs_root"
	StorageRevokedDuringDeploy     = "storage_revoked_during_deploy"
	StorageNotAttachableByNewOwner = "storage_not_attachable_by_new_owner"
)

// PlannedStorage is one mapping the plan names.
type PlannedStorage struct {
	ResourceID string `json:"-"`
	Name       string `json:"name"`
	Kind       string `json:"kind,omitempty"`
	Folder     string `json:"folder,omitempty"`
	Access     string `json:"access"`
	Path       string `json:"path"`
	// WasAccess and WasPath are the table's, for a Change.
	WasAccess string `json:"was_access,omitempty"`
	WasPath   string `json:"was_path,omitempty"`
	// Authority is the stamp the flip writes: the owner's delegation.
	Authority string `json:"-"`
}

func (p PlannedStorage) label() string {
	if p.Folder == "" {
		return p.Name
	}
	return p.Name + "/" + p.Folder
}

// StorageProblem is one refusal of the plan.
type StorageProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Hint is the next step when the judge knows one better than the code's
	// (codeHint): an administrator refused gets the routes an administrator
	// has, not "ask the resource's administrator".
	Hint string `json:"hint,omitempty"`
}

// StoragePlan is toft.toml's storage against the table: what the flip keeps,
// adds, changes and removes. Console mappings the toml declares identically
// are listed apart — kept, and never adopted.
type StoragePlan struct {
	Keep    []PlannedStorage `json:"keep"`
	Add     []PlannedStorage `json:"add"`
	Change  []PlannedStorage `json:"change"`
	Remove  []PlannedStorage `json:"remove"`
	Console []PlannedStorage `json:"console,omitempty"`
	// Problems refuse the deploy; empty when the plan may be carried out.
	Problems []StorageProblem `json:"-"`
	// Mounts is what the new release's workload mounts — every console mapping
	// and the plan's keep, add and change — once the planner has prepared it.
	Mounts []runtime.Mount `json:"-"`
}

// Writes reports whether the flip has anything to write.
func (p *StoragePlan) Writes() bool {
	return p != nil && len(p.Add)+len(p.Change)+len(p.Remove) > 0
}

// Lines are the plan as the build log and `toft check` print it.
func (p *StoragePlan) Lines() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, s := range p.Keep {
		out = append(out, fmt.Sprintf("= %s, %s at %s", s.label(), s.Access, s.Path))
	}
	for _, s := range p.Console {
		out = append(out, fmt.Sprintf("= %s, kept: attached in the console", s.label()))
	}
	for _, s := range p.Add {
		out = append(out, fmt.Sprintf("+ %s, %s at %s", s.label(), s.Access, s.Path))
	}
	for _, s := range p.Change {
		out = append(out, fmt.Sprintf("~ %s, %s at %s (was %s at %s)", s.label(), s.Access, s.Path, s.WasAccess, s.WasPath))
	}
	for _, s := range p.Remove {
		out = append(out, fmt.Sprintf("- %s, detached (its files are kept)", s.label()))
	}
	return out
}

// Deployer is who asked for a deploy: database.Deployer* and an id, or empty
// for one no principal asked for (auto, render, move).
type Deployer struct {
	Type string
	ID   string
}

// DeployerOf reads the principal off a deployment row.
func DeployerOf(d *database.Deployment) Deployer {
	if d == nil {
		return Deployer{}
	}
	return Deployer{Type: d.DeployedByType, ID: d.DeployedByID}
}

// StoragePlanRequest is what the planner judges.
type StoragePlanRequest struct {
	// App is the app — for a check before its create, a sketch carrying the
	// owner it would have and its framework, with no id.
	App *database.App
	// Entries are toft.toml's, nil when it declares no `[[storage]]` — which
	// removes every manifest mapping.
	Entries  []StorageEntry
	Deployer Deployer
}

// StoragePlanner is the half of manifest storage that needs the API server:
// the console's validator, the delegations, the bindings. The builder reaches
// it the way it reaches the runtimes, through an interface set at wiring.
type StoragePlanner interface {
	// PlanManifestStorage judges the entries against the table, the owner and
	// the deployer. An error is the platform failing; a refusal is a Problem.
	PlanManifestStorage(req StoragePlanRequest) (*StoragePlan, error)
	// PrepareStoragePlan makes the bindings the plan's entries need,
	// and fills plan.Mounts — before the new release starts.
	PrepareStoragePlan(ctx context.Context, app *database.App, plan *StoragePlan) error
	// ReconcileManifestStorage writes the plan at the flip, re-checking the
	// owner's delegations under lock. A delegation gone or lowered since the
	// plan is database.ErrStorageRevokedDuringDeploy.
	ReconcileManifestStorage(app *database.App, plan *StoragePlan, deployer Deployer) error
}

// SetStoragePlanner wires manifest storage. Without one, a `[[storage]]` table
// is refused: storage this install cannot judge is not storage it attaches.
func (b *Builder) SetStoragePlanner(p StoragePlanner) { b.storagePlanner = p }

// planStorage is checkManifest's half for `[[storage]]`: plan, log the plan,
// and refuse with every problem at once.
func (b *Builder) planStorage(app *database.App, deploy *database.Deployment, decl ManifestDecl, appendLog func(string)) (*StoragePlan, error) {
	if b.storagePlanner == nil {
		if decl.StorageDeclared {
			return nil, &storageRefusal{problems: []StorageProblem{{Code: StorageUnknown,
				Message: "this install does not offer storage, so toft.toml's [[storage]] cannot be attached"}}}
		}
		return nil, nil
	}
	plan, err := b.storagePlanner.PlanManifestStorage(StoragePlanRequest{
		App: app, Entries: decl.Storage, Deployer: DeployerOf(deploy)})
	if err != nil {
		return nil, fmt.Errorf("planning toft.toml storage: %w", err)
	}
	if len(plan.Problems) > 0 {
		return nil, &storageRefusal{problems: plan.Problems}
	}
	if lines := plan.Lines(); len(lines) > 0 {
		appendLog("==> Storage declared in toft.toml:")
		for _, l := range lines {
			appendLog("    " + l)
		}
	}
	return plan, nil
}

// storageRefusal is the deploy's failure for a refused plan: every problem, one
// per line, so the first fix does not reveal the second.
type storageRefusal struct{ problems []StorageProblem }

func (e *storageRefusal) Error() string {
	if len(e.problems) == 1 {
		return ManifestFile + " [[storage]]: " + e.problems[0].Message
	}
	lines := make([]string, 0, len(e.problems))
	for _, p := range e.problems {
		lines = append(lines, "  - "+p.Message)
	}
	return ManifestFile + " [[storage]] is refused:\n" + strings.Join(lines, "\n")
}
