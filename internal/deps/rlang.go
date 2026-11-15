package deps

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"toft/internal/toolchain"

	"toft/internal/advisory"
)

// renv.lock, and the ecosystem that runs no command.
//
// Both existing ecosystems need one, because neither of their manifests is a
// lockfile: npm resolves package.json over the registry, and uv compiles a
// requirements.txt into pinned versions. `renv.lock` already *is* the resolved
// answer — JSON, holding every package, its exact version, its repository and
// its hash — so R's resolver parses instead of executing.
//
// That is a first for this package, and it is the ecosystem's own property
// rather than a shortcut. It also makes R the only ecosystem where *the scan
// happens before any dependency code runs* is true with no caveat: Python's
// `uv pip compile` may execute a setup.py, and npm's fallback resolves metadata
// over the network. R's reads a file.

// renvLock is the part of the lockfile this reads. Every other key — the R
// version, the repository list, each package's hash and its remote fields — is
// ignored rather than rejected: the file is renv's, its shape is renv's to
// extend, and a reader that refused an unfamiliar key would fail a deploy over
// a field it had no use for.
type renvLock struct {
	Packages map[string]struct {
		Package    string `json:"Package"`
		Version    string `json:"Version"`
		Source     string `json:"Source"`
		Repository string `json:"Repository"`
	} `json:"Packages"`
}

// resolveCRAN reads a release's renv.lock into the inventory.
//
// No command, so no context and no timeout: there is nothing to hang. The
// signature keeps the shape of its siblings so the dispatch in Resolve reads
// the same for every ecosystem.
func resolveCRAN(dir string, logf Logf) ([]Dependency, []string, error) {
	data, err := toolchain.ReadRegularFile(filepath.Join(dir, "renv.lock"), toolchain.MaxManifestBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("reading renv.lock: %w", err)
	}

	var lock renvLock
	if err := json.Unmarshal(data, &lock); err != nil {
		// A malformed lockfile is a refusal rather than an empty inventory.
		// Reporting no dependencies for a file the platform could not read
		// would be a status that is a lie, and the deploy is the moment the
		// author can act on it.
		return nil, nil, fmt.Errorf("renv.lock could not be parsed as JSON: %w", err)
	}

	deps := make([]Dependency, 0, len(lock.Packages))
	var skipped []string
	for key, p := range lock.Packages {
		name := p.Package
		if name == "" {
			name = key
		}
		if p.Version == "" {
			// A package with no version cannot be matched against anything, so
			// recording it would put a row in the inventory that no advisory
			// can ever apply to.
			skipped = append(skipped, name)
			continue
		}
		deps = append(deps, Dependency{
			Ecosystem: advisory.EcosystemCRAN,
			Package:   name,
			Version:   p.Version,
			// renv.lock is a flat, fully-resolved set: it records every package
			// that will be installed and does not distinguish the ones the
			// author asked for from the ones those pulled in. Claiming a
			// direct/transitive split we cannot see would be worse than not
			// having one, so every package is recorded the same way and the
			// Path column stays empty.
			Direct: true,
		})
	}

	sort.Slice(deps, func(i, j int) bool { return deps[i].Package < deps[j].Package })

	var notes []string
	if len(skipped) > 0 {
		sort.Strings(skipped)
		notes = append(notes, fmt.Sprintf("renv.lock names %d package(s) with no version, which "+
			"cannot be checked against advisories: %v", len(skipped), skipped))
	}
	logf("    renv.lock: %d packages", len(deps))
	return deps, notes, nil
}

// RenvLockPackages is the set of package names a release's renv.lock lists, or
// an empty set when it has none or it cannot be parsed — the reader asks only
// whether the app chose a package, and Step 2 has already refused a lockfile it
// could not read.
func RenvLockPackages(dir string) map[string]bool {
	names := map[string]bool{}
	data, err := toolchain.ReadRegularFile(filepath.Join(dir, "renv.lock"), toolchain.MaxManifestBytes)
	if err != nil {
		return names
	}
	var lock renvLock
	if json.Unmarshal(data, &lock) != nil {
		return names
	}
	for key, p := range lock.Packages {
		if p.Package != "" {
			names[p.Package] = true
		} else {
			names[key] = true
		}
	}
	return names
}
