package deps

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"toft/internal/advisory"
	"toft/internal/toolchain"
)

// An installed R library, read the way renv.lock is: as files, with no R
// process.
//
// renv.lock says what a build *will* install. The library says what it *did*,
// and it is the only record of the packages nothing names in a lockfile — the
// ones Toft installs into every R release whatever the app said, and the ones
// an app installs into its own library while it runs. Each installed package
// is a directory holding a DESCRIPTION file in Debian control format, and that
// file is the whole of what this reads: `Package`, `Version` and `Priority`.
//
// Reading it rather than asking R keeps the property resolveCRAN has: R is the
// one ecosystem whose scan executes nothing.

// ReadRLibrary lists the packages installed in an R library directory.
//
// A package whose Priority is `base` is skipped: it is R itself, versioned
// with the interpreter, and no CRAN advisory names it. A directory with no
// DESCRIPTION is not a package (renv's own staging directories, a lock
// directory left by an interrupted install) and is passed over silently. One
// whose DESCRIPTION has no Package or Version is counted in a note — the
// treatment resolveCRAN gives a lockfile entry with no version.
//
// A library that does not exist is no packages and no error: a release whose
// build installed nothing has none. Every row is cran, Direct false, and
// carries no Source — the caller knows which source it is reading for.
func ReadRLibrary(dir string) ([]Dependency, []string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the R library: %w", err)
	}

	var out []Dependency
	var unreadable []string
	for _, e := range entries {
		// A symlink is followed only into a directory: renv links a package
		// out of its cache in some configurations, and the link is still the
		// package the app loads.
		path := filepath.Join(dir, e.Name())
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			continue
		}
		// Regular, bounded and non-blocking: the library is in a release the
		// app's account owns (toolchain.ReadRegularFile).
		data, err := toolchain.ReadRegularFile(filepath.Join(path, "DESCRIPTION"), toolchain.MaxManifestBytes)
		if err != nil {
			continue
		}
		fields := parseDCF(data)
		if fields["Priority"] == "base" {
			continue
		}
		name, version := fields["Package"], fields["Version"]
		if name == "" || version == "" {
			unreadable = append(unreadable, e.Name())
			continue
		}
		out = append(out, Dependency{
			Ecosystem: advisory.EcosystemCRAN,
			Package:   name,
			Version:   version,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })

	var notes []string
	if len(unreadable) > 0 {
		sort.Strings(unreadable)
		notes = append(notes, fmt.Sprintf("%d installed R package(s) have no readable name and "+
			"version, so they cannot be checked against advisories: %v", len(unreadable), unreadable))
	}
	return out, notes, nil
}

// RLibraryDirs is every per-series library a release holds:
// `<release>/renv/library/R-<series>`, which is where the build installs and
// where R_LIBS_SITE points a running app. More than one exists only when an
// image moved its R under a release and a repair built a second.
func RLibraryDirs(releaseDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(releaseDir, "renv", "library", "R-*"))
	sort.Strings(matches)
	return matches
}

// parseDCF reads the first record of a Debian control file. A line starting
// with whitespace continues the previous field; the first blank line ends the
// record. Values are trimmed.
func parseDCF(data []byte) map[string]string {
	fields := map[string]string{}
	var last string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			if len(fields) > 0 {
				break
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if last != "" {
				fields[last] += " " + strings.TrimSpace(line)
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			last = ""
			continue
		}
		last = strings.TrimSpace(k)
		fields[last] = strings.TrimSpace(v)
	}
	return fields
}
