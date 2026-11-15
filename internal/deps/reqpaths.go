package deps

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"toft/internal/toolchain"
)

// requirementPathOptions are the requirements-file options whose value uv
// reads as a path on this machine.
var requirementPathOptions = []string{
	"--requirement", "--constraint", "--editable", "--find-links",
	"-r", "-c", "-e", "-f",
}

// outsideRequirementPath is the refusal for a requirements file in dir — or a
// file it includes — that names a path outside dir, or that cannot be read
// the way uv will read it; nil when every path stays inside.
//
// **uv runs here as the platform, before the handover**, and reads what it is
// pointed at: `-r /etc/shadow` was read as root and uv's parse error, quoting
// the file, landed in the owner's build log (security review 2026-11-10,
// E-2). The links that could do the same are gone by now
// (builder.dropEscapingLinks); this is the same rule for a path written in
// the file itself. A URL is not a path, except `file:`.
//
// The file is read whole, bounded, and tokenised the way uv tokenises it: an
// option that ends its line takes its value from the first token of the next
// line (`-r` NEWLINE `sub/../../../etc/hostname` made uv read the host file,
// code review 2026-11-11, G-1), and a trailing backslash joins the next line.
// Anything the walk cannot read — a line it cannot hold, a FIFO, a file over
// the manifest cap — is refused rather than skipped, because uv would still
// read it.
func outsideRequirementPath(dir, name string) error {
	return walkRequirements(dir, name, map[string]bool{}, 0)
}

func walkRequirements(dir, name string, seen map[string]bool, depth int) error {
	if seen[name] || depth > 16 {
		return nil
	}
	seen[name] = true
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer root.Close()
	f, err := root.OpenFile(name, os.O_RDONLY|toolchain.OpenNonblock, 0)
	if err != nil {
		// Absent: uv's own error, with no path read. An included file that is
		// not there is uv's problem too.
		return nil
	}
	defer f.Close()
	data, err := toolchain.ReadRegular(f, name, toolchain.MaxManifestBytes)
	if err != nil {
		return refuseUnreadableRequirements(name, err)
	}
	here := path.Dir(filepath.ToSlash(name))
	// pendingOpt is an include/path option that ended its line without a
	// value; uv gives it the next line's first token.
	pendingOpt := ""
	for _, line := range logicalLines(string(data)) {
		fields := strings.Fields(line)
		for i := 0; i < len(fields); i++ {
			tok := fields[i]
			if strings.HasPrefix(tok, "#") {
				break
			}
			value, opt, ok := "", "", false
			if pendingOpt != "" {
				value, opt, ok = tok, pendingOpt, true
				pendingOpt = ""
			} else {
				value, opt, ok = pathOption(fields, &i)
				if ok && value == "" {
					// The option is the last token on its line.
					pendingOpt = opt
					continue
				}
			}
			include := false
			if ok {
				include = opt == "-r" || opt == "-c" || opt == "--requirement" || opt == "--constraint"
			} else if i == 0 && (strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, ".") || strings.HasPrefix(tok, "file:")) {
				value = tok
			} else {
				continue
			}
			rel, inside := insideRelease(here, value)
			if !inside {
				return refuseOutsideRequirements(name, value)
			}
			if include && rel != "" {
				if err := walkRequirements(dir, rel, seen, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// pathOption reads fields[*i] as one of requirementPathOptions: the option,
// its value, and whether it was one. A value given as the next field advances
// *i past it; an option with no value on its line comes back with "" — the
// caller takes the next line's first token, as uv does.
func pathOption(fields []string, i *int) (value, opt string, ok bool) {
	tok := fields[*i]
	for _, opt := range requirementPathOptions {
		switch {
		case tok == opt:
			if *i+1 < len(fields) {
				*i++
				return fields[*i], opt, true
			}
			return "", opt, true
		case strings.HasPrefix(tok, opt+"="):
			return tok[len(opt)+1:], opt, true
		case len(opt) == 2 && strings.HasPrefix(tok, opt) && len(tok) > 2 && !strings.HasPrefix(tok, "--"):
			return tok[2:], opt, true
		}
	}
	return "", "", false
}

// logicalLines splits a requirements file into lines, joining a line that
// ends with a backslash to the next one, as pip does. uv itself fails such a
// file; joining errs toward refusing what uv would have refused anyway.
func logicalLines(text string) []string {
	var out []string
	carry := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if trimmed := strings.TrimRight(line, " \t"); strings.HasSuffix(trimmed, "\\") {
			carry += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}
		out = append(out, carry+line)
		carry = ""
	}
	if carry != "" {
		out = append(out, carry)
	}
	return out
}

// insideRelease reads value, written in a file in the release folder here, as
// a path: its release-relative form and whether it stays inside. A URL that
// is not file: stays "inside" with no path, because it is not read here.
func insideRelease(here, value string) (string, bool) {
	if strings.HasPrefix(value, "file:") {
		value = strings.TrimPrefix(strings.TrimPrefix(value, "file:"), "//")
		if strings.HasPrefix(value, "/") {
			return "", false
		}
	} else if strings.Contains(value, "://") {
		return "", true
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") || strings.ContainsAny(value, "$") {
		return "", false
	}
	rel := path.Clean(path.Join(here, value))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// refuseOutsideRequirements is the error resolvePyPI returns for such a path.
func refuseOutsideRequirements(file, value string) error {
	return fmt.Errorf("%s names %q, which is outside the repository; a requirements file "+
		"may only include, constrain or install from paths inside it", file, value)
}

// refuseUnreadableRequirements is the error for a requirements file the walk
// could not check — not a regular file, or over the manifest cap.
func refuseUnreadableRequirements(file string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("%s could not be checked before installing from it (%v); a requirements "+
		"file must be a regular file under %d MiB", file, err, toolchain.MaxManifestBytes>>20)
}
