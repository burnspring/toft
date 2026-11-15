package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/joho/godotenv"

	"toft/cli/internal/ui"
)

// `toft env` — the app's environment variables.
//
// **`set` and `unset` are one call to the merge route**, `PATCH
// /apps/{id}/env`, which names only the keys it changes. Nothing is read
// first: the route leaves every unmentioned key alone, decides `--only-missing`
// per key on the install, and refuses a caller who may not edit the app. So
// there is no window for a concurrent edit to another key to be lost, and a
// value this command did not send is never seen by it.
//
// Values may come from a dotenv file or stdin (`--from-file`), parsed here so
// a malformed line refuses before anything is sent, and so a secret manager
// can pipe values in without them touching argv.

const envUsage = `toft env — the app's environment variables

Usage:
  toft env list                      Names, and values that are not sensitive
  toft env set KEY=VALUE [...]       Set one or more; other variables are kept
  toft env set --from-file FILE      Set every variable in a dotenv file (- is stdin)
  toft env unset KEY [...]           Remove one or more

Flags:
  --app <slug>        Which app
  --from-file <file>  With set: read KEY=VALUE lines from a dotenv file, or from
                      stdin with -. KEY=VALUE operands win over the file
  --sensitive         With set: store every value write-only. It is never shown
                      again, by this command or by the Settings page
  --visible           With set: store every value readable by the app's editors
  --only-missing      With set: leave a variable that already has a value as it is
  --json              Machine-readable output on stdout

Without --sensitive or --visible a variable keeps its flag, and a new one is
visible. KEY= clears a variable in place. A value is taken as typed, split on
the first "=". A variable an administrator attached through a credential cannot
be set here; the install says which credential supplies it. A change reaches
the app when it next starts: ` + "`toft restart`" + ` applies it.
`

// envVar is one variable as the app's detail route returns it.
type envVar struct {
	Key       string `json:"key"`
	Value     string `json:"value,omitempty"`
	Sensitive bool   `json:"sensitive"`
	// Unset is a sensitive variable holding nothing — "not set", most often a
	// secret toft.toml declared that nobody has filled yet.
	Unset bool `json:"unset,omitempty"`
}

type appEnv struct {
	Keys []string `json:"env_keys"`
	Vars []envVar `json:"env_vars"`
}

func cmdEnv(e *env, args []string) error {
	if len(args) == 0 {
		ui.Help(os.Stderr, envUsage)
		return usageCodef("missing_argument", "toft env needs a subcommand: list, set or unset")
	}
	switch args[0] {
	case "list":
		return envList(e, args[1:])
	case "set":
		return envChange(e, "set", args[1:])
	case "unset":
		return envChange(e, "unset", args[1:])
	case "-h", "--help", "help":
		ui.Help(os.Stdout, envUsage)
		return nil
	}
	return unknownSubcommand("env", args, "list, set or unset")
}

func (s *session) appEnv(appID string) (*appEnv, error) {
	var body appEnv
	if err := s.client.Do(http.MethodGet, "/api/v1/apps/"+appID, nil, &body); err != nil {
		return nil, err
	}
	return &body, nil
}

func envList(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("env list", envUsage)
	g.register(fs)
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	got, err := s.appEnv(app.ID)
	if err != nil {
		return err
	}
	vars := envView(got)
	if g.json {
		return writeNDJSON(e.out.Stdout, vars)
	}
	if len(vars) == 0 {
		e.out.Say("%s has no environment variables.", app.Slug)
		return nil
	}
	if e.out.Rich() {
		t := e.out.NewTable("key", "value")
		for _, v := range vars {
			switch {
			case v.Sensitive && v.Unset:
				t.Row(e.out.Bold(v.Key), e.out.Dimmed("not set"))
			case v.Sensitive:
				t.Row(e.out.Bold(v.Key), e.out.Dimmed("•••••••• sensitive"))
			case v.readable:
				// Another editor's text: its controls shown as `?`
				// (security review 2026-11-15, CL-6).
				t.Row(e.out.Bold(v.Key), listedName(v.Value))
			default:
				t.Row(e.out.Bold(v.Key), e.out.Dimmed("—"))
			}
		}
		t.Print()
		if !vars[0].readable {
			e.out.Dim("Values are shown only to somebody who may edit %s.", app.Slug)
		}
		return nil
	}
	for _, v := range vars {
		switch {
		case v.Sensitive && v.Unset:
			e.out.Answer("%s  %s", v.Key, e.out.Dimmed("not set"))
		case v.Sensitive:
			e.out.Answer("%s=%s", v.Key, e.out.Dimmed("(sensitive)"))
		case v.readable:
			// The stored value to a pipe, which may be reading it back;
			// cleaned on a terminal, which --plain still reaches.
			value := v.Value
			if writesToTerminal(e.out.Stdout) {
				value = listedName(value)
			}
			e.out.Answer("%s=%s", v.Key, value)
		default:
			e.out.Answer("%s", v.Key)
		}
	}
	if !vars[0].readable {
		e.out.Dim("Values are shown only to somebody who may edit %s.", app.Slug)
	}
	return nil
}

// envViewVar is a variable as list prints it. `readable` is whether this
// credential was shown values at all, so a visible variable whose value is the
// empty string is not mistaken for one this caller may not read.
type envViewVar struct {
	envVar
	readable bool
}

// MarshalJSON is the --json row. **`value` is present exactly when it was
// shown**, the empty string included: a variable set to "" used to lose the
// key to omitempty and read the same as one whose value was withheld. A
// sensitive one has `"sensitive": true` and no value; a caller who may not
// read values gets the key alone.
func (v envViewVar) MarshalJSON() ([]byte, error) {
	switch {
	case !v.readable:
		return json.Marshal(map[string]string{"key": v.Key})
	case v.Sensitive && v.Unset:
		return json.Marshal(map[string]any{"key": v.Key, "sensitive": true, "unset": true})
	case v.Sensitive:
		return json.Marshal(map[string]any{"key": v.Key, "sensitive": true})
	}
	return json.Marshal(map[string]any{"key": v.Key, "value": v.Value, "sensitive": false})
}

func envView(got *appEnv) []envViewVar {
	// Under --json the rows are NDJSON, one per line, and none is no lines
	// (BB-035, fourth 2026-11-03 list).
	out := []envViewVar{}
	seen := map[string]bool{}
	for _, v := range got.Vars {
		out = append(out, envViewVar{envVar: v, readable: true})
		seen[v.Key] = true
	}
	for _, k := range got.Keys {
		if !seen[k] {
			out = append(out, envViewVar{envVar: envVar{Key: k}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// envChangeResult is the merge route's answer: what happened per key, never
// a value.
type envChangeResult struct {
	Set        []string `json:"set"`
	Unchanged  []string `json:"unchanged"`
	Kept       []string `json:"kept"`
	Cleared    []string `json:"cleared"`
	Unset      []string `json:"unset"`
	NotPresent []string `json:"not_present"`
}

func envChange(e *env, verb string, args []string) error {
	var g globalFlags
	fs := newFlags("env "+verb, envUsage)
	g.register(fs)
	sensitive := fs.Bool("sensitive", false, "store the values write-only")
	visible := fs.Bool("visible", false, "store the values readable by the app's editors")
	onlyMissing := fs.Bool("only-missing", false, "leave a variable that is already set as it is")
	fromFile := fs.String("from-file", "", "read KEY=VALUE lines from a dotenv file, or stdin with -")
	flags, operands := partitionOperands(args)
	// --restart was removed when a set stopped restarting the app; name the
	// command that does it rather than refusing it as an unknown flag.
	for _, f := range flags {
		if name, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "="); name == "restart" && strings.HasPrefix(f, "-") {
			return usageCodef("removed_flag", "--restart is gone: toft env %s stores the value and the app reads it at its "+
				"next start — run toft restart to apply it now (a document app — notebook, Quarto, "+
				"R Markdown — reads it at its next deploy instead, and a static app reads none)", verb)
		}
	}
	if err := parse(fs, flags); err != nil {
		return skipHelp(err)
	}
	if verb != "set" && (*sensitive || *visible || *onlyMissing || *fromFile != "") {
		return usageCodef("conflicting_flags", "--sensitive, --visible, --only-missing and --from-file go with set")
	}
	if *sensitive && *visible {
		return usageCodef("conflicting_flags", "--sensitive and --visible say opposite things; pick one")
	}
	if len(operands) == 0 && *fromFile == "" {
		if verb == "set" {
			return usageCodef("missing_argument", "toft env set needs KEY=VALUE or --from-file")
		}
		return usageCodef("missing_argument", "toft env unset needs a KEY")
	}

	values := map[string]string{}
	var order []string
	add := func(key, value string) error {
		if key == "" {
			return usageCodef("invalid_env_name", "a variable has no name before the =")
		}
		if verb == "set" {
			if err := checkEnvName(key); err != nil {
				return err
			}
		}
		if _, dup := values[key]; !dup {
			order = append(order, key)
		}
		values[key] = value
		return nil
	}
	if *fromFile != "" {
		file, err := readDotenv(e, *fromFile)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(file))
		for k := range file {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := add(k, file[k]); err != nil {
				return err
			}
		}
	}
	for _, op := range operands {
		key, value := op, ""
		if verb == "set" {
			var ok bool
			key, value, ok = strings.Cut(op, "=")
			if !ok {
				return usageCodef("invalid_assignment", "%q is not KEY=VALUE", op)
			}
		}
		if err := add(key, value); err != nil {
			return err
		}
	}
	if len(order) == 0 && *fromFile == "-" {
		// Nothing on stdin is empty_input, as every stdin reader here says
		// it (L10, 2026-11-04 black-box list).
		return usageHinted("empty_input", "pipe KEY=VALUE lines in: `printf 'KEY=value\\n' | toft env set --from-file -`",
			"stdin held no variables")
	}
	if len(order) == 0 {
		return usageCodef("invalid_env_file", "%s holds no variables", *fromFile)
	}

	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	if verb == "set" {
		if warning := envSetWarning(values); warning != "" {
			e.out.Warn("%s", warning)
		}
	}
	body := map[string]any{}
	if verb == "unset" {
		body["unset"] = order
	} else {
		set := map[string]any{}
		for _, k := range order {
			one := map[string]any{"value": values[k]}
			if *sensitive || *visible {
				one["sensitive"] = *sensitive
			}
			set[k] = one
		}
		body["set"] = set
		body["only_missing"] = *onlyMissing
	}
	var res envChangeResult
	if err := s.client.Do(http.MethodPatch, "/api/v1/apps/"+app.ID+"/env", body, &res); err != nil {
		return err
	}
	if verb == "unset" && len(res.Unset) == 0 && len(res.NotPresent) > 0 {
		return usageCodef("env_not_set", "%s has no variable %s — `toft env list` names them", app.Slug, strings.Join(res.NotPresent, ", "))
	}

	// A document reads its environment when it is rendered, which is at
	// deploy: a restart serves the same pages again (D11).
	// A static app has no process at all: nothing reads the variable, and
	// neither a restart nor a deploy changes that (drills 2026-10-28 #8).
	document := isDocumentFramework(app.Framework)
	static := app.Framework == "static"
	// The route says what it wrote and nothing about a restart, which is
	// `toft restart`'s; whether one is worth naming is this client's call.
	changed := len(res.Set)+len(res.Cleared)+len(res.Unset) > 0
	if g.json {
		doc := map[string]any{"app": app.Slug}
		for k, v := range map[string][]string{"set": res.Set, "unchanged": res.Unchanged, "kept": res.Kept, "cleared": res.Cleared,
			"unset": res.Unset, "not_present": res.NotPresent} {
			if len(v) > 0 {
				doc[k] = v
			}
		}
		if verb == "set" && doc["set"] == nil {
			doc["set"] = []string{}
		}
		if document {
			doc["redeploy_needed"] = changed
		}
		if changed && static {
			doc["note"] = staticEnvNote
		}
		if warning := envSetWarning(values); verb == "set" && warning != "" {
			doc["warnings"] = []string{warning}
		}
		return json.NewEncoder(e.out.Stdout).Encode(doc)
	}
	var said []string
	if len(res.Set) > 0 {
		said = append(said, strings.Join(res.Set, ", ")+" set")
	}
	if len(res.Cleared) > 0 {
		said = append(said, strings.Join(res.Cleared, ", ")+" cleared")
	}
	if len(res.Unset) > 0 {
		said = append(said, strings.Join(res.Unset, ", ")+" removed")
	}
	if len(said) > 0 {
		e.out.Answer("%s on %s", strings.Join(said, "; "), app.Slug)
	} else {
		e.out.Answer("nothing changed on %s", app.Slug)
	}
	if len(res.Unchanged) > 0 {
		e.out.Dim("%s already had that value.", strings.Join(res.Unchanged, ", "))
	}
	if len(res.Kept) > 0 {
		e.out.Dim("%s kept (already set): --only-missing leaves a set variable as it is.", strings.Join(res.Kept, ", "))
	}
	if len(res.NotPresent) > 0 {
		e.out.Dim("%s was not set, so there was nothing to remove.", strings.Join(res.NotPresent, ", "))
	}
	switch {
	case !changed:
	case static:
		e.out.Dim("%s", staticEnvNote)
	case document:
		e.out.Dim("A document reads its environment when it is rendered: the next deploy " +
			"(`toft redeploy`) applies this to the page, and a job reads it at its next start.")
	default:
		e.out.Dim("The app reads its environment when it starts: `toft restart` applies this.")
	}
	return nil
}

// readDotenv parses a dotenv file, or stdin for "-". A malformed file refuses
// naming its first bad line, before anything is sent.
func readDotenv(e *env, path string) (map[string]string, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = readAllCtx(e.ctx, e.input())
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, usageCodef("unreadable_file", "reading %s: %v", path, err)
	}
	vars, err := godotenv.Unmarshal(string(raw))
	if err == nil {
		if msg := dotenvExpansionProblem(string(raw), vars); msg != "" {
			name := path
			if path == "-" {
				name = "stdin"
			}
			return nil, usageCodef("invalid_env_file", "%s: %s", name, msg)
		}
		return vars, nil
	}
	// The parser names no line, so find the first one that fails alone. A
	// quoted value spanning lines fails alone too, which is the right answer
	// only when the whole file failed — and it did.
	name := path
	if path == "-" {
		name = "stdin"
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The line number and at most the name before its separator — not
		// the parser's sentence, which names the newline it appended for
		// `not a line` (drills 2026-10-30) and quotes source text, and not the
		// line, whose value is the secret this file keeps off argv (security
		// review 2026-11-15, CL-1).
		if _, lineErr := godotenv.Unmarshal(line + "\n"); lineErr != nil {
			return nil, usageCodef("invalid_env_file", "%s line %d %s", name, i+1, badLineName(line))
		}
	}
	return nil, usageCodef("invalid_env_file", "%s is not a dotenv file", name)
}

// badLineName is what a refusal may say about a line the parser refused: the
// text before its first `=` or `:`, trimmed and capped, never what follows.
func badLineName(line string) string {
	i := strings.IndexAny(line, "=:")
	if i < 0 {
		return "has no KEY= in it"
	}
	key := strings.TrimSpace(line[:i])
	if len(key) > 40 {
		key = strings.ToValidUTF8(key[:40], "") + "…"
	}
	return fmt.Sprintf("is not KEY=VALUE (its name is %q)", key)
}

// envSetWarning is a value that is legal and a trap (BB-047): NODE_ENV as an
// app variable reaches the build, where npm reads production as --omit=dev
// and the build fails on a missing devDependency. The platform already sets
// it for the running process. Stderr only — the set still happens.
func envSetWarning(values map[string]string) string {
	if v, ok := values["NODE_ENV"]; ok && strings.EqualFold(strings.TrimSpace(v), "production") {
		return "NODE_ENV=production reaches the build too, where npm skips devDependencies and a " +
			"Node build fails on the missing one (vite: command not found). The platform already " +
			"sets it for the running app — `toft env unset NODE_ENV` removes this one."
	}
	return ""
}

// staticEnvNote is `toft env set` on a static app, with no restart asked.
const staticEnvNote = "A static app's files are served as they are, and no process of its own reads an " +
	"environment: this variable reaches nothing in it."

// isDocumentFramework is the frameworks rendered once, at deploy, and then
// served as files: a notebook, Quarto, R Markdown. The install's own list is
// restartNote's (internal/api); this is a copy, because the CLI is its own
// module.
func isDocumentFramework(f string) bool {
	switch f {
	case "notebook", "quarto", "rmarkdown":
		return true
	}
	return false
}

// partitionOperands separates KEY=VALUE and KEY operands from the flags, so
// `toft env set A=1 --sensitive` reads the way it is typed.
//
// It predates parseOperands reading flags anywhere and is kept because an
// operand here can never start with "-", so the split is unambiguous. The
// flags that take a value as the next argument are the two global ones and
// --from-file, whose "-" is stdin rather than a flag.
// envName is what a process environment can carry and a shell can export.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// platformSetEnv are the variables the platform sets on an app's process over
// the app's own, so a value stored under one never arrives. The install's list
// is runner.PlatformEnvNames; this is a copy, because the CLI is its own module,
// and the install refuses the same names whichever list a client carries.
// TestPlatformEnvNamesMatchTheClientsCopy (internal/runner) is the drift guard.
var platformSetEnv = map[string]bool{
	"PORT": true, "HOST": true, "BASE_PATH": true,
	"TMPDIR": true, "TOFT_SCRATCH_DIR": true,
	"VIRTUAL_ENV": true, "R_LIBS_SITE": true,
	"TOFT_API_URL": true, "TOFT_APP_TOKEN": true, "TOFT_APP_ID": true, "TOFT_APP_SLUG": true,
	"TOFT_ROLE_KEY": true, "TOFT_STORAGE": true,
	"TOFT_JOB_TOKEN": true, "TOFT_RUN_ID": true, "TOFT_JOB_NAME": true, "TOFT_TRIGGER": true,
	"TOFT_TRIGGER_ROLE": true, "TOFT_OUTPUT_URI": true,
	"TOFT_SCHEDULED_FOR": true, "TOFT_PARAMS": true, "TOFT_RERUN_OF": true, "TOFT_DEADLINE": true,
}

// checkEnvName refuses a name before the round trip — the install validates
// too, and this is the fast path that makes the mistake exit 2 with no request.
func checkEnvName(key string) error {
	if !envName.MatchString(key) {
		return usageCodef("invalid_env_name", "%q is not a variable name — letters, digits and "+
			"underscores, not starting with a digit", key)
	}
	if platformSetEnv[key] {
		return usageCodef("reserved_env_name", "%s is set by the platform on every app — "+
			"read it, but it cannot be set here", key)
	}
	return nil
}

func partitionOperands(args []string) (flags, operands []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return flags, append(operands, args[i+1:]...)
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
			if (a == "--app" || a == "-app" || a == "--install" || a == "-install" ||
				a == "--from-file" || a == "-from-file") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			operands = append(operands, a)
		}
	}
	return flags, operands
}

// dotenvExpansionProblem names the first key whose value the dotenv parser
// rewrote: in an unquoted or double-quoted value `$NAME` is expanded against
// the file's own earlier keys and an unknown name becomes "", so a secret
// piped in as `DB_PASSWORD=aB3$Kq9` was stored as `aB3q9`, the CLI said "set",
// and a sensitive value cannot be read back to notice (code review
// 2026-11-11, E-7). The operand form takes a value as typed; the file form
// refuses rather than altering it, and says how to write it literally.
//
// The comparison is the parser's own, not a line reader's: godotenv also takes
// `:` as a separator, any blank after `export`, several statements on one
// line and a double-quoted value spanning lines, and a second reader of the
// text missed every one of them (security review 2026-11-15, CL-1). So the
// file is parsed again with every `$` replaced by a rune it does not hold,
// which nothing expands, and the `$` put back: a value that differs from the
// first parse is one the parser rewrote. Neither value is named — the file
// exists to keep secrets out of sight, and the key says which line it is.
func dotenvExpansionProblem(raw string, parsed map[string]string) string {
	if !strings.Contains(raw, "$") {
		return ""
	}
	r := rune(0xE000)
	for strings.ContainsRune(raw, r) {
		r++
	}
	sentinel := string(r)
	literal, err := godotenv.Unmarshal(strings.ReplaceAll(raw, "$", sentinel))
	if err != nil {
		// The text parsed once; a parse that fails only with `$` hidden is
		// itself a value the parser treats specially. Name no key.
		return "has a value holding $, which a dotenv file expands — write it in single quotes to keep it literal"
	}
	keys := make([]string, 0, len(parsed))
	for k := range parsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.ReplaceAll(literal[key], sentinel, "$") != parsed[key] {
			return fmt.Sprintf("the value of %s contains $, which a dotenv file expands — "+
				"write it in single quotes to keep it literal, or pass %s=... as an operand", key, key)
		}
	}
	return ""
}
