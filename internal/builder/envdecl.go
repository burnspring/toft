package builder

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"toft/internal/database"
	"toft/internal/runner"
)

// toft.toml [env] — the variables an app's code needs, one key each.
//
//	[env]
//	DB_HOST     = "db.internal"          # visible, with a default
//	API_URL     = {}                     # visible, no default
//	DB_PASSWORD = { sensitive = true }   # sensitive, never a default
//
// The author decides which variables are secrets. A visible variable may carry
// a default, which every install starts from; a sensitive one may not, because
// a value in the repository is in every bundle, release directory, archive and
// promotion, readable by every editor everywhere. A secret's value reaches each
// install through PATCH /apps/{id}/env (`toft env set`) instead (ADR §14).

// Codes of the [env] refusals. The two name codes are runner.EnvNameProblem's,
// so a client that handles the API's refusals handles these.
const (
	EnvSensitiveDefaultCode = "sensitive_default"
	EnvEntryInvalidCode     = "env_entry_invalid"
)

// envSecretHint is the cure for a secret given a value in the file.
const envSecretHint = "declare it as { sensitive = true } and send its value with `toft env set --from-file`"

// EnvDecl is one variable toft.toml declares.
type EnvDecl struct {
	Name      string
	Default   string
	Sensitive bool
}

// envDeclsFrom is the [env] table's variables, sorted by name, or the first
// refusal (also in name order, so the same file gets the same refusal).
func envDeclsFrom(raw any) ([]EnvDecl, error) {
	var errs []error
	if raw == nil {
		return nil, nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, ManifestError{Field: "env", Code: EnvEntryInvalidCode,
			Msg: `must be a table of variables, like DB_HOST = "db.internal"`}
	}
	names := make([]string, 0, len(table))
	for k := range table {
		names = append(names, k)
	}
	sort.Strings(names)
	decls := make([]EnvDecl, 0, len(names))
vars:
	for _, name := range names {
		field := "env." + name
		if code, msg := runner.EnvNameProblem(name); code != "" {
			errs = append(errs, ManifestError{Field: field, Code: code, Msg: msg})
			continue vars
		}
		d := EnvDecl{Name: name}
		switch v := table[name].(type) {
		case string:
			d.Default = v
		case map[string]any:
			hasDefault := false
			for k, x := range v {
				switch k {
				case "sensitive":
					b, ok := x.(bool)
					if !ok {
						errs = append(errs, ManifestError{Field: field + ".sensitive", Code: EnvEntryInvalidCode,
							Msg: "must be true or false"})
						continue vars
					}
					d.Sensitive = b
				case "default":
					str, ok := x.(string)
					if !ok {
						errs = append(errs, ManifestError{Field: field + ".default", Code: EnvEntryInvalidCode,
							Msg: "must be a string"})
						continue vars
					}
					d.Default, hasDefault = str, true
				default:
					errs = append(errs, ManifestError{Field: field + "." + k, Code: EnvEntryInvalidCode,
						Msg: "is not read: a variable takes sensitive and default"})
					continue vars
				}
			}
			if d.Sensitive && hasDefault {
				errs = append(errs, ManifestError{Field: field, Code: EnvSensitiveDefaultCode,
					Msg: "is sensitive and may not have a default in the repository", Hint: envSecretHint})
				continue vars
			}
		default:
			errs = append(errs, ManifestError{Field: field, Code: EnvEntryInvalidCode,
				Msg: `must be a string default or a table, like { sensitive = true }`})
			continue vars
		}
		decls = append(decls, d)
	}
	return decls, joinManifestErrors(errs)
}

// ParseEnvDecls reads a release's declared environment. A missing file or
// table is none; a refusal is ParseManifestWithWarnings's own, which has
// already failed the deploy by the time configure asks.
func ParseEnvDecls(releaseDir string) ([]EnvDecl, error) {
	data, err := readInRelease(releaseDir, ManifestFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ManifestFile, err)
	}
	var m struct {
		Env any `toml:"env"`
	}
	if _, err := toml.Decode(string(data), &m); err != nil {
		return nil, tomlDecodeError(string(data), err)
	}
	return envDeclsFrom(m.Env)
}

// applyEnvDecls seeds every declared variable the app has no row for, with
// its default and the flag the file gave it. Add-only — a stored row is never
// touched, value or flag, and a variable the file stopped declaring is never
// deleted. The opposite of the jobs reconcile on purpose: a job is code the
// file owns, a variable's value is an operator's work once it exists.
//
// At configure rather than at the flip, because the release's process starts
// before the flip and reads its environment then. A name an attached
// credential supplies is skipped and said — not refused, since the variable
// the file asked for exists, and an administrator attaching a credential
// must not break an owner's next deploy.
func (b *Builder) applyEnvDecls(app *database.App, releaseDir string, appendLog func(string)) error {
	decls, err := ParseEnvDecls(releaseDir)
	if err != nil {
		return err
	}
	if len(decls) == 0 {
		return nil
	}
	var added []string
	kept := 0
	for _, d := range decls {
		name := d.Name
		encrypted, err := b.encryptor.Encrypt(d.Default)
		if err != nil {
			return fmt.Errorf("encrypting %s: %w", name, err)
		}
		inserted, err := b.db.AddEnvVarIfAbsent(app.ID, name, encrypted, d.Sensitive)
		var fromCred *database.EnvKeyFromCredentialError
		switch {
		case errors.As(err, &fromCred):
			appendLog(fmt.Sprintf("==> %s [env]: %s is supplied by the %q credential", ManifestFile, name, fromCred.Credential))
			continue
		case err != nil:
			return fmt.Errorf("storing %s: %w", name, err)
		case inserted:
			added = append(added, name)
		default:
			kept++
		}
	}
	what := "added nothing"
	if len(added) > 0 {
		what = "added " + strings.Join(added, ", ")
	}
	appendLog(fmt.Sprintf("==> %s [env]: %s (%d declared, %d already set)", ManifestFile, what, len(decls), kept))
	return nil
}
