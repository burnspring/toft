package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The API server's half of toft.toml's `[[storage]]` (ADR § Storage somebody
// else can attach): the builder hands it the entries, and it judges them with
// the console's own validator and the delegations — the owner's for every
// entry, the deployer's for a change.

// storageDeployer is the deployer resolved to what D3 and D4 ask of it.
type storageDeployer struct {
	// personID is whose delegations a change is checked against: a user, or a
	// service account's owning user. Empty when no person asked.
	personID string
	// keyMayAttach is a deploy key an administrator let add storage: a change
	// passes on the owner's right alone (D19).
	keyMayAttach bool
	// isKey says the deployer was a deploy key, for the refusal's second line.
	isKey bool
}

func (s *Server) resolveStorageDeployer(d builder.Deployer) storageDeployer {
	switch d.Type {
	case database.DeployerUser, database.DeployerServiceAccount:
		return storageDeployer{personID: d.ID}
	case database.DeployerDeployKey:
		out := storageDeployer{isKey: true}
		if k, err := s.db.GetDeployKey(d.ID); err == nil && k != nil {
			out.keyMayAttach = k.MayAttach
		}
		return out
	}
	return storageDeployer{}
}

// PlanManifestStorage is builder.StoragePlanner's judge. Every entry is judged
// and every refusal returned, so the first fix does not reveal the second.
func (s *Server) PlanManifestStorage(req builder.StoragePlanRequest) (*builder.StoragePlan, error) {
	plan := &builder.StoragePlan{Keep: []builder.PlannedStorage{}, Add: []builder.PlannedStorage{},
		Change: []builder.PlannedStorage{}, Remove: []builder.PlannedStorage{}}
	app := req.App
	refuse := func(code, msg string) {
		plan.Problems = append(plan.Problems, builder.StorageProblem{Code: code, Message: msg})
	}

	var table []database.StorageMapping
	if app.ID != "" {
		var err error
		if table, err = s.db.ListStorageMappingsForApp(app.ID); err != nil {
			return nil, fmt.Errorf("reading this app's storage: %w", err)
		}
	}
	key := func(resourceID, folder string) string { return resourceID + "\x00" + folder }
	byKey := map[string]database.StorageMapping{}
	consoleCount := 0
	for _, m := range table {
		byKey[key(m.ResourceID, m.Folder)] = m
		if m.Source != database.MappingSourceManifest {
			consoleCount++
		}
	}

	if len(req.Entries) > 0 && !s.storageOffered() {
		refuse(builder.StorageUnknown, "this install does not offer storage, so toft.toml's [[storage]] cannot be attached")
		return plan, nil
	}

	deployer := s.resolveStorageDeployer(req.Deployer)
	declared := map[string]bool{}
	for _, e := range req.Entries {
		p, problem, err := s.judgeStorageEntry(app, e, deployer, byKey)
		if err != nil {
			return nil, err
		}
		if problem.Code != "" {
			plan.Problems = append(plan.Problems, problem)
			continue
		}
		declared[key(p.ResourceID, p.Folder)] = true
		existing, has := byKey[key(p.ResourceID, p.Folder)]
		switch {
		case has && existing.Source != database.MappingSourceManifest:
			plan.Console = append(plan.Console, p)
		case !has:
			plan.Add = append(plan.Add, p)
		case existing.Access == p.Access && existing.Path == p.Path && existing.Authority == p.Authority:
			plan.Keep = append(plan.Keep, p)
		default:
			p.WasAccess, p.WasPath = existing.Access, existing.Path
			plan.Change = append(plan.Change, p)
		}
	}
	for _, m := range table {
		if m.Source == database.MappingSourceManifest && !declared[key(m.ResourceID, m.Folder)] {
			plan.Remove = append(plan.Remove, builder.PlannedStorage{ResourceID: m.ResourceID,
				Name: m.ResourceSlug, Kind: m.ResourceKind, Folder: m.Folder, Access: m.Access, Path: m.Path})
		}
	}

	// A path is the app's, whichever half wrote it.
	for _, list := range [][]builder.PlannedStorage{plan.Add, plan.Change} {
		for _, p := range list {
			for _, m := range table {
				if m.Source != database.MappingSourceManifest && m.Path == p.Path &&
					(m.ResourceID != p.ResourceID || m.Folder != p.Folder) {
					refuse(builder.StoragePathTaken, fmt.Sprintf(
						"%s is mounted at %s, where this app already mounts %s, attached in the console — "+
							"give the entry a path of its own", p.Name, p.Path, m.ResourceSlug))
				}
				if m.Source != database.MappingSourceManifest && database.MountPathsNest(m.Path, p.Path) {
					refuse(builder.StoragePathTaken, fmt.Sprintf(
						"%s is mounted at %s, which nests with %s, where this app mounts %s, attached in the "+
							"console — one mount cannot sit inside another; give the entry a path beside it",
						p.Name, p.Path, m.Path, m.ResourceSlug))
				}
			}
		}
	}
	// The cap counts console and manifest together: it bounds the mounts.
	if total := consoleCount + len(plan.Keep) + len(plan.Add) + len(plan.Change); total > database.MaxAppStorageMappings {
		refuse(attachCapReached, fmt.Sprintf("this app would hold %d storage mappings, and %s",
			total, capRefusal().Message))
	}
	return plan, nil
}

// judgeStorageEntry is one entry against the owner, the deployer and the
// console's validator. It answers the planned mapping, or a code and the
// sentence (PRD §5).
func (s *Server) judgeStorageEntry(app *database.App, e builder.StorageEntry, dep storageDeployer,
	table map[string]database.StorageMapping) (builder.PlannedStorage, builder.StorageProblem, error) {
	var none builder.PlannedStorage
	unknown := fmt.Sprintf("%q is not storage you can attach. `toft storage ls` lists what you can.", e.Name)

	res, deleting, err := s.db.StorageResourceByName(e.Name)
	if err != nil {
		return none, builder.StorageProblem{}, fmt.Errorf("reading storage %q: %w", e.Name, err)
	}
	if res == nil {
		// A name being deleted reads as unknown too: the tombstone carries no
		// delegations to check, and "being deleted" against "unknown" told a
		// deployer who held nothing on it which names exist (code review
		// 2026-11-11, B-3). The deletion ends soon, and then it *is* unknown.
		_ = deleting
		return none, builder.StorageProblem{Code: builder.StorageUnknown, Message: unknown}, nil
	}

	owner, ownerOK, err := s.db.OwnerAttachAuthority(app, res.ID)
	if err != nil {
		return none, builder.StorageProblem{}, fmt.Errorf("resolving the owner's delegation on %q: %w", e.Name, err)
	}
	var caller database.StorageDelegation
	callerOK := false
	if dep.personID != "" {
		if caller, callerOK, err = s.db.AttachAuthority(dep.personID, res.ID); err != nil {
			return none, builder.StorageProblem{}, fmt.Errorf("resolving the deployer's delegation on %q: %w", e.Name, err)
		}
	}

	// The console's own rules, normalizing the folder before the table is
	// asked about it. The partition overlap must not count the app's own row
	// at the same folder, which PartitionConflict already does not.
	entry, refusal, err := s.validateAttach(app, res,
		attachEntry{Path: e.Path, Access: e.Access, Folder: e.Folder}, attachOptions{ExceptManifestOf: app.ID})
	if err != nil {
		return none, builder.StorageProblem{}, err
	}

	existing, has := table[res.ID+"\x00"+entry.Folder]
	if has && existing.Source != database.MappingSourceManifest {
		if existing.Access == entry.Access && existing.Path == entry.Path {
			// Attached in the console exactly as declared: kept, never adopted.
			return builder.PlannedStorage{ResourceID: res.ID, Name: res.Slug, Kind: res.Kind,
				Folder: entry.Folder, Access: entry.Access, Path: entry.Path, Authority: existing.Authority}, builder.StorageProblem{}, nil
		}
		return none, builder.StorageProblem{Code: builder.StorageConflictsConsole, Message: fmt.Sprintf(
			"%s is attached in the console at %s (%s), and toft.toml declares it at %s (%s). "+
				"Detach it in the console to let toft.toml own it, or declare it as it is attached.",
			labelOf(res.Slug, entry.Folder), existing.Path, existing.Access, entry.Path, entry.Access)}, nil
	}
	// A change is new, or higher than the table's; a narrowed level, a moved
	// path and a removal are not (D3).
	change := !has || !database.StoreAccessAtLeast(existing.Access, entry.Access)
	label := labelOf(res.Slug, entry.Folder)

	if change {
		switch {
		case dep.personID == "" && !dep.keyMayAttach && !ownerOK:
			// Nobody on this app holds anything on it, and the key may not
			// attach: the same answer as a name that does not exist, before
			// anything names the resource (B-3). A key that may attach is
			// trusted with the install's storage, and is told.
			return none, builder.StorageProblem{Code: builder.StorageUnknown, Message: unknown}, nil
		case dep.personID == "" && !dep.keyMayAttach:
			msg := fmt.Sprintf("a deploy with no person can keep storage but not add it — run `toft deploy` "+
				"once as someone who can attach %s", label)
			if dep.isKey {
				msg += ", or ask an administrator to let this deploy key attach storage"
			}
			return none, builder.StorageProblem{Code: builder.StorageRequiresPerson, Message: msg}, nil
		case dep.personID != "" && !callerOK && !ownerOK:
			return s.unknownOrGrantOnly(dep.personID, app.ID, res, e, unknown)
		case dep.personID != "" && !callerOK:
			return none, builder.StorageProblem{Code: builder.StorageNotAttachableByYou, Message: fmt.Sprintf(
				"this app's owner can attach %s, but you cannot, and adding it or raising its level is a "+
					"change you have to be able to make — an entry already attached at this level would have passed",
				label)}, nil
		}
	}
	if !ownerOK {
		if dep.personID != "" && !callerOK {
			return s.unknownOrGrantOnly(dep.personID, app.ID, res, e, unknown)
		}
		return none, builder.StorageProblem{Code: builder.StorageNotAttachableByOwner, Message: s.ownerCannotAttach(app, res, caller, callerOK)}, nil
	}
	if refusal != nil {
		return none, builder.StorageProblem{Code: refusal.Code, Message: refusal.Message}, nil
	}
	if change && dep.personID != "" && !database.StoreAccessAtLeast(caller.MaxAccess, entry.Access) {
		return none, builder.StorageProblem{Code: builder.StorageLevelAboveCeiling, Message: fmt.Sprintf(
			"you may attach %q at %s at most, not %s", res.Slug, caller.MaxAccess, entry.Access)}, nil
	}
	if !database.StoreAccessAtLeast(owner.MaxAccess, entry.Access) {
		return none, builder.StorageProblem{Code: builder.StorageLevelAboveCeiling, Message: fmt.Sprintf(
			"this app's owner, %s, may attach %q at %s at most, not %s",
			s.ownerName(app), res.Slug, owner.MaxAccess, entry.Access)}, nil
	}
	return builder.PlannedStorage{ResourceID: res.ID, Name: res.Slug, Kind: res.Kind,
		Folder: entry.Folder, Access: entry.Access, Path: entry.Path, Authority: owner.AuthorityKey()}, builder.StorageProblem{}, nil
}

// unknownOrGrantOnly is the refusal for a person who cannot attach a resource
// nobody on this app can attach either. A person who reaches it from outside —
// a store grant, `toft store` — is told the one thing that grant is not: an
// attach right (BB-004). Anyone else gets storage_unknown, the same answer as a
// name that does not exist, so a resource the caller holds nothing on is never
// confirmed to exist.
func (s *Server) unknownOrGrantOnly(personID, appID string, res *database.StorageResource, e builder.StorageEntry, unknown string) (
	builder.PlannedStorage, builder.StorageProblem, error) {
	// An administrator sees every resource, so "not storage you can attach"
	// read as "does not exist" to one who had just created it. Being an
	// administrator is not a delegation — attaching through toft.toml is the
	// owner's and the deployer's — so the refusal stands and says the two
	// ways through (porting feedback 2026-11-03).
	if u, err := s.db.GetUser(personID); err == nil && u != nil && u.SystemRole == database.SystemRoleAdmin {
		// The route suggested is one this install will take: the level the
		// resource's root serves where it lives under one — a read-write root
		// serves full and nothing else, so the "read" this used to suggest was
		// refused — and the hint is the administrator's own two routes, not
		// "ask the resource's administrator" (BB-032, fourth 2026-11-03
		// black-box list).
		access := e.Access
		if root, ok := s.sharedRootOf(res); ok {
			access = runtime.AccessFull
			if root.ReadOnly {
				access = runtime.AccessRead
			}
		} else if access == "" {
			access = runtime.AccessRead
		}
		path := e.Path
		if path == "" {
			path = builder.DefaultStoragePath(res.Slug, e.Folder)
		}
		// json.Marshal, never %q, which is not JSON escaping (AS-7).
		encoded, _ := json.Marshal(struct {
			Path   string `json:"path"`
			Access string `json:"access"`
			Folder string `json:"folder,omitempty"`
		}{path, string(access), e.Folder})
		body := string(encoded)
		// The app's own id where there is one: the literal <app-id> led a
		// reader with the app linked and known nowhere (BB-008). A check
		// against a tree with no app yet keeps the placeholder.
		target := appID
		if target == "" {
			target = "<app-id>"
		}
		attach := fmt.Sprintf("toft api PUT admin/storage/%s/mappings/%s --data '%s'", res.ID, target, body)
		return builder.PlannedStorage{}, builder.StorageProblem{Code: builder.StorageNotAttachableByYou, Message: fmt.Sprintf(
			"%q exists, but being an administrator is not a right to attach it through toft.toml — that "+
				"takes a delegation to manage it, to you or to this app's owner (PUT "+
				"/api/v1/admin/storage/%s/managers, from a signed-in session). Or attach it from the "+
				"administrator's side and leave it out of toft.toml: `%s`.", res.Slug, res.ID, attach),
			Hint: "attach it from the administrator's side — " + attach + " — and remove the [[storage]] " +
				"entry, or delegate it to yourself or the app's owner on Admin → Storage and deploy again"}, nil
	}
	scopes, err := s.db.StoreScopesBySlug(personID, res.Slug)
	if err != nil {
		return builder.PlannedStorage{}, builder.StorageProblem{}, fmt.Errorf("reading your reach on %q: %w", res.Slug, err)
	}
	if scopes == nil {
		return builder.PlannedStorage{}, builder.StorageProblem{Code: builder.StorageUnknown, Message: unknown}, nil
	}
	return builder.PlannedStorage{}, builder.StorageProblem{Code: builder.StorageNotAttachableByYou, Message: fmt.Sprintf(
		"you can reach %q from outside (a store grant, `toft store`), but a grant is not a right to "+
			"attach it to an app — attaching takes a delegation to manage it, which an administrator "+
			"gives to you or to this app's owner. `toft storage ls` lists what you can attach.", res.Slug)}, nil
}

func labelOf(name, folder string) string {
	if folder == "" {
		return fmt.Sprintf("%q", name)
	}
	return fmt.Sprintf("folder %q of %q", folder, name)
}

// ownerName is the owner as a refusal names it: `group finance-apps`, or a
// person's login.
func (s *Server) ownerName(app *database.App) string {
	if app.OwnerGroupID != "" {
		if g, _ := s.db.GetGroup(app.OwnerGroupID); g != nil {
			return "group " + g.Name
		}
		return "its owning group"
	}
	if u, _ := s.db.GetUser(app.OwnerID); u != nil {
		return u.Login
	}
	return "its owner"
}

// ownerCannotAttach is D16's legible refusal: which half is missing.
func (s *Server) ownerCannotAttach(app *database.App, res *database.StorageResource,
	caller database.StorageDelegation, callerOK bool) string {
	through := ""
	if callerOK {
		through = " directly"
		if caller.SubjectType == database.StoreSubjectGroup {
			if g, _ := s.db.GetGroup(caller.SubjectID); g != nil {
				through = " (through " + g.Name + ")"
			}
		}
	}
	lead := ""
	if callerOK {
		lead = fmt.Sprintf("You can attach %q%s, but ", res.Slug, through)
	}
	owner := s.ownerName(app)
	msg := fmt.Sprintf("%sthis app's owner, %s, cannot. A mapping declared in toft.toml is held by the "+
		"app's owner, so ask an administrator to let %s attach %q, or remove it from toft.toml.",
		lead, owner, owner, res.Slug)
	if callerOK && app.OwnerGroupID != "" && caller.SubjectType == database.StoreSubjectGroup &&
		caller.SubjectID != app.OwnerGroupID {
		msg += " A group-owned app attaches through its owning group's own delegation, not its members'."
	}
	if !callerOK {
		// No person to tell what they can do; the owner is the whole answer.
		msg = upperFirst(msg)
	}
	return msg
}

func upperFirst(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-32) + s[1:]
}

// manifestWant is the plan's mappings the flip holds: keep, add and change.
func manifestWant(plan *builder.StoragePlan) []database.ManifestMapping {
	var out []database.ManifestMapping
	for _, list := range [][]builder.PlannedStorage{plan.Keep, plan.Add, plan.Change} {
		for _, p := range list {
			out = append(out, database.ManifestMapping{ResourceID: p.ResourceID, Folder: p.Folder,
				Path: p.Path, Access: p.Access, Authority: p.Authority})
		}
	}
	return out
}

// PrepareStoragePlan makes what the plan's entries need before the
// new release starts — the binding for each level and folder, and a
// partitioned host folder's gate — and fills plan.Mounts.
//
// The driver's error stays in the log, as on the console's delegate route:
// the deployer is not an operator.
func (s *Server) PrepareStoragePlan(ctx context.Context, app *database.App, plan *builder.StoragePlan) error {
	// Kept entries are ensured too (AP-3): the call is idempotent, and a
	// redeploy is then what repairs a binding released from under a mapping.
	if s.storage != nil {
		for _, p := range plan.Keep {
			res, err := s.db.GetStorageResource(p.ResourceID)
			if err != nil || res == nil {
				return fmt.Errorf("%q is no longer on this install", p.Name)
			}
			if err := s.ensureBinding(ctx, res, p.Access, p.Folder); err != nil {
				log.Printf("binding storage %s at %s for %s: %v", res.Slug, p.Access, app.Slug, err)
				return fmt.Errorf("this install could not bind %q — ask an administrator to look at the platform log", p.Name)
			}
		}
	}
	for _, list := range [][]builder.PlannedStorage{plan.Add, plan.Change} {
		for _, p := range list {
			res, err := s.db.GetStorageResource(p.ResourceID)
			if err != nil || res == nil {
				return fmt.Errorf("%q is no longer on this install", p.Name)
			}
			if s.storage != nil {
				if err := s.ensureBinding(ctx, res, p.Access, p.Folder); err != nil {
					log.Printf("binding storage %s at %s for %s: %v", res.Slug, p.Access, app.Slug, err)
					return fmt.Errorf("this install could not bind %q — ask an administrator to look at the platform log", p.Name)
				}
			}
			if err := s.db.EnsureFolderGate(res.ID, p.Folder); err != nil {
				if errors.Is(err, database.ErrGateRangeExhausted) {
					return fmt.Errorf("%v — an operator widens SHARED_STORAGE_GID_RANGE", err)
				}
				if errors.Is(err, database.ErrGateNameTaken) {
					return fmt.Errorf("folder %q of %q: %v", p.Folder, p.Name, err)
				}
				return fmt.Errorf("numbering the gate for %q: %w", p.Name, err)
			}
		}
	}
	mounts, err := s.db.PlannedMounts(app.ID, manifestWant(plan))
	if err != nil {
		return fmt.Errorf("assembling the planned mounts: %w", err)
	}
	plan.Mounts = mounts
	return nil
}

// ReconcileManifestStorage is the flip's write. Each planned stamp is
// re-resolved against the owner as the app is *now* — a transfer since the
// plan moves the stamp with it — and the database locks those rows and
// refuses if any went (database.ErrStorageRevokedDuringDeploy). What it
// removed is swept, as a console detach's is.
func (s *Server) ReconcileManifestStorage(app *database.App, plan *builder.StoragePlan, deployer builder.Deployer) error {
	fresh, err := s.db.GetApp(app.ID)
	if err != nil || fresh == nil {
		return fmt.Errorf("reading the app at the flip: %v", err)
	}
	want := manifestWant(plan)
	adding := map[string]bool{}
	for _, list := range [][]builder.PlannedStorage{plan.Add, plan.Change} {
		for _, p := range list {
			adding[p.ResourceID+"\x00"+p.Folder] = true
		}
	}
	var lost []string
	for i := range want {
		d, ok, err := s.db.OwnerAttachAuthority(fresh, want[i].ResourceID)
		if err != nil {
			return err
		}
		if ok {
			want[i].Authority = d.AuthorityKey()
			continue
		}
		// Not ok. For a kept row the stamp stays the plan's, and the lock
		// below finds the row gone and refuses when it was revoked. For an
		// Add or Change the owner has to hold the delegation *now*: the plan's
		// stamp was still a live row when the owner had merely left the group,
		// or the app had been transferred, so the lock passed and the mapping
		// was written under a delegation the owner did not hold (code review
		// 2026-11-11, B-4).
		if adding[want[i].ResourceID+"\x00"+want[i].Folder] {
			var slug string
			if res, err := s.db.GetStorageResource(want[i].ResourceID); err == nil && res != nil {
				slug = res.Slug
			}
			lost = append(lost, slug)
		}
	}
	if len(lost) > 0 {
		return &database.StorageRevokedError{Resources: lost}
	}
	actorType, actorID := database.PrincipalTypeApp, app.ID
	switch deployer.Type {
	case database.DeployerUser, database.DeployerServiceAccount:
		actorType, actorID = database.PrincipalTypeUser, deployer.ID
	case database.DeployerDeployKey:
		actorType, actorID = auth.PrincipalTypeDeployKey, deployer.ID
	}
	removed, err := s.db.ReconcileManifestStorage(app.ID, want, actorType, actorID)
	if err != nil {
		return err
	}
	byResource := map[string][]database.StorageMapping{}
	for _, m := range removed {
		byResource[m.ResourceID] = append(byResource[m.ResourceID], m)
	}
	for resourceID, ms := range byResource {
		s.sweepBindings(context.Background(), resourceID, ms)
	}
	return nil
}
