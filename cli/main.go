// Command toft is the Toft command-line client.
//
// The platform has one door and it is a browser, which is right for the
// administrator surfaces and wrong for the loop this product exists to shorten:
// write code, see it running, write more. `toft deploy` bundles the working
// directory as it is — uncommitted edits included — and pushes it, for every
// app, whether it was created from an archive or from a repository.
//
// Eighteen commands. Command dispatch and help are hand-rolled on `flag` rather
// than taken from a framework, because the help text is half of what "usable"
// means for a tool like this and a framework's generated help is the part every
// CLI ends up fighting.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"toft/cli/internal/bundle"
	"toft/cli/internal/client"
	"toft/cli/internal/config"
	"toft/cli/internal/ui"
)

// version is the published version of this client. Overridable at build time,
// like the server's:
//
//	go build -ldflags="-X main.version=0.2.0" ./cli
var version = "0.2.0"

// build is an opaque id for the build this binary came from, stamped by
// scripts/publish-cli.sh alongside the version.
//
// **The version number alone cannot answer "am I current".** A published asset
// was once rebuilt in place under an existing tag: two binaries, different
// bytes, both answering `0.5.1`, and the only way to tell them apart was a
// checksum against SHA256SUMS. Version comparison — the check anything
// automated reaches for first — reported "current" for a stale binary.
//
// A digest of the source rather than the commit hash, because the published
// matrix builds with -buildvcs=false to keep a private repository's hashes and
// timestamps out of a public binary; the id answers *is this the same build*
// without disclosing which build it is.
//
// Empty for an ordinary `go build`, and omitted from the output when empty
// rather than printed as a placeholder: a local build is a normal build, not a
// broken one.
var build = ""

func versionLine() string {
	if build == "" {
		return "toft " + version
	}
	return "toft " + version + " (build " + build + ")"
}

// Exit codes are a contract, documented and tested.
//
// `3` and `4` exist so a red pipeline can be triaged without reading the log:
// "the deploy failed", "the token expired" and "the install was unreachable"
// are three different pages.
const (
	exitOK = 0
	// exitFailed is the platform doing the thing and it failing — a failed
	// build, a scan that refused the bundle. The interesting case, and the one
	// a CI job wants distinguishable from every other kind of red.
	exitFailed = 1
	// exitUsage is usage or configuration: no app linked, a bad flag, a prompt
	// refused because nothing could answer it.
	exitUsage = 2
	// exitAuth is not authenticated, or refused: no credential, a revoked key,
	// uploads switched off — or the app's state refusing (a 409 that is not a
	// taken name or a wait): the serving deployment, a batch app asked to start.
	exitAuth = 3
	// exitUnreachable is the install not answering.
	exitUnreachable = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitUsage
	}

	switch first := firstAfterGlobals(args); first {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return exitOK
	case "-v", "--version", "version":
		fmt.Println(versionLine())
		return exitOK
	}

	commands := commandTable()

	// Both refusals here come before there is an env, and are still said the
	// way every other one is: a red ✗ on a terminal, `toft: ` off one.
	early := func() *ui.Out {
		plain := plainFromEnv()
		return ui.New(isTerminal(os.Stdout) && !plain, !plain, false)
	}

	// `toft --json apps` is `toft apps --json`: the global flags may come
	// before the command (BB-039), and are handed to it as if written after.
	leading, rest := leadingGlobalFlags(args)
	if len(rest) == 0 {
		err := usageCodef("no_command", "no command after %s — run `toft --help` for the list",
			strings.Join(leading, " "))
		out := early()
		out.Error(err)
		if wantsJSON(leading) {
			(&env{out: out, json: true}).errorDocument(err, "usage", "no_command", exitUsage)
		}
		return exitUsage
	}
	if err := switchGivenAValue(leading, rest); err != nil {
		out := early()
		out.Error(err)
		// Its hint, on a plain run as under --json (L5, 2026-11-04 list).
		var u *usageError
		if errors.As(err, &u) && u.hint != "" && !out.Rich() {
			out.Hint("%s", sentenceCase(u.hint))
		}
		// --json anywhere on the line, not only among the leading flags:
		// `toft --plain false apps --json` is a --json caller too (BB-063,
		// fifth 2026-11-03 list).
		if wantsJSON(args) {
			(&env{out: out, json: true}).errorDocument(err, "usage", "bad_flag", exitUsage)
		}
		return exitUsage
	}
	// A command with subcommands parses its flags after the subcommand word,
	// so that is where the leading ones go: `toft --json env list` is
	// `toft env list --json`, not `toft env --json list` (BB-001).
	at := 1
	if groupedCommands[rest[0]] && len(rest) > 1 && !strings.HasPrefix(rest[1], "-") {
		at = 2
	}
	args = append(append(append([]string{}, rest[:at]...), leading...), rest[at:]...)

	command, ok := commands[args[0]]
	if !ok {
		err := usageCodef("unknown_command", "unknown command %q — run `toft --help` for the list", args[0])
		out := early()
		out.Error(err)
		// A --json caller anywhere on the line gets the refusal as a
		// document, like every other one (BB-039).
		if wantsJSON(args[1:]) {
			(&env{out: out, json: true}).errorDocument(err, "usage", "unknown_command", exitUsage)
			return exitUsage
		}
		fmt.Fprintln(os.Stderr)
		usage(os.Stderr)
		return exitUsage
	}

	e, err := newEnv()
	if err != nil {
		early().Error(err)
		return exitUsage
	}
	// args[0] is the command; wantsJSON reads the flags after it.
	e.json = wantsJSON(args[1:])
	e.command = strings.Join(rest[:at], " ")
	defer e.stop()

	return report(e, command(e, args[1:]))
}

// leadingGlobalFlags splits off the global flags written before the command —
// `toft --json --app demo status` — and returns them with what follows. Only
// the global flags: anything else in front of the command is left where it is,
// and is then refused as an unknown command naming what was typed.
func leadingGlobalFlags(args []string) (leading, rest []string) {
	valued := map[string]bool{"app": true, "install": true}
	switches := map[string]bool{"json": true, "quiet": true, "q": true, "yes": true,
		"no-color": true, "plain": true, "session": true}
	i := 0
	for i < len(args) {
		a := args[i]
		if len(a) < 2 || a[0] != '-' || a == "--" {
			break
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			if n := name[:eq]; valued[n] || switches[n] {
				leading = append(leading, a)
				i++
				continue
			}
			break
		}
		switch {
		case switches[name]:
			leading = append(leading, a)
			i++
		case valued[name] && i+1 < len(args):
			leading = append(leading, a, args[i+1])
			i += 2
		default:
			return leading, args[i:]
		}
	}
	return leading, args[i:]
}

// groupedCommands are the commands whose first argument is a subcommand.
// TestGroupedCommandsAreListed keeps this beside the dispatch it describes.
var groupedCommands = map[string]bool{
	"env": true, "job": true, "deployments": true, "store": true, "storage": true, "sdk": true,
}

// switchGivenAValue refuses a leading switch followed by true or false —
// `toft --plain false apps` — the way a switch after the command is refused:
// a switch does not read the word after it, so `false` would otherwise be
// taken for the command (BB-001).
func switchGivenAValue(leading, rest []string) error {
	if len(leading) == 0 || len(rest) == 0 {
		return nil
	}
	last := leading[len(leading)-1]
	if strings.Contains(last, "=") {
		return nil
	}
	name := strings.TrimLeft(last, "-")
	if name == "app" || name == "install" {
		return nil
	}
	if len(leading) >= 2 {
		if prev := strings.TrimLeft(leading[len(leading)-2], "-"); (prev == "app" || prev == "install") &&
			!strings.Contains(leading[len(leading)-2], "=") {
			return nil // last is that flag's value
		}
	}
	if word := strings.ToLower(rest[0]); word == "true" || word == "false" {
		return usageHinted("bad_flag", fmt.Sprintf("write %s=%s — a switch takes its value after =", last, word),
			"%s is a switch and does not read the word after it, so %q was not its value", last, rest[0])
	}
	return nil
}

// firstAfterGlobals is the first word that is not a leading global flag, or "".
func firstAfterGlobals(args []string) string {
	if _, rest := leadingGlobalFlags(args); len(rest) > 0 {
		return rest[0]
	}
	return ""
}

// wantsJSON reports whether `--json` is among the raw arguments.
//
// The exact flag, in either of the two spellings Go's `flag` accepts, anywhere
// before a bare `--`. A value that merely contains the word — a slug, a path — is
// not it.
//
// **It deliberately does not try to tell a flag's value from a positional.**
// Doing so would mean knowing which flags take one, which is a second list of
// every command's flags and exactly the thing that goes stale: the first version
// of this stopped at the first non-dash word and so read `--app demo --json` as
// not asking for JSON, which is the single most likely way anybody writes it.
//
// What that costs is a contrived `--name --json`, where `flag` would consume
// `--json` as the value and this would not. It is a guess, it is consulted only
// for refusals that happen before any command parses anything, and `setup()`
// replaces it with the parsed answer the moment there is one — so the worst case
// is one machine-readable refusal for a run that spelled its intent oddly.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" || a == "-json" {
			return true
		}
	}
	return false
}

// commandTable is dispatch, in one place a test can read.
//
// A function rather than a package-level variable so that the closures it names
// are resolved at call time, and so that a test asking "what commands exist"
// gets the same answer `run` does — a second, hand-kept list is how a command
// ships without documentation or a --json answer and nothing notices.
func commandTable() map[string]func(*env, []string) error {
	return map[string]func(*env, []string) error{
		"login":    cmdLogin,
		"link":     cmdLink,
		"deploy":   cmdDeploy,
		"deps":     cmdDeps,
		"status":   cmdStatus,
		"logs":     cmdLogs,
		"rollback": cmdRollback,
		"apps":     cmdApps,
		"top":      cmdTop,
		"store":    cmdStore,
		"storage":  cmdStorage,
		"sdk":      cmdSDK,
		"create":   cmdCreate,
		"events":   cmdEvents,
		"job":      cmdJob,
		"api":      cmdAPI,
		"task":     cmdTask,
		"redeploy": cmdRedeploy,
		"restart":  cmdRestart,
		"start":    cmdStart,
		"stop":     cmdStop,
		// What used to be `toft api` and a hand-written body.
		"env":         cmdEnv,
		"visibility":  cmdVisibility,
		"hostname":    cmdHostname,
		"delete":      cmdDelete,
		"deployments": cmdDeployments,
		// Asking before doing: what the app answers, what a deploy would
		// decide, what this credential and install allow, and the skills.
		"fetch":  cmdFetch,
		"check":  cmdCheck,
		"whoami": cmdWhoami,
		"skill":  cmdSkill,
	}
}

// report turns an error into an exit code and a sentence.
//
// One place, so the mapping is a contract rather than a habit that drifts: a
// command returns an error and never calls os.Exit itself.
//
// Under `--json` the sentence is *also* a document on stdout. Everything a
// command succeeds at is already machine-readable there; a refusal was the one
// outcome a program had to read English off stderr to understand, which meant
// the ordinary way to handle a failed `toft` was to not handle it. The exception
// is a deploy that failed, which has already written a complete result object —
// a second document restating it would be two answers to one question.
func report(e *env, err error) int {
	if err == nil {
		return exitOK
	}
	// The writer is built by setup(), and a command can fail before it gets
	// there: a flag refused at parse time is the ordinary case, and it is the
	// one a person hits by typing. Reporting an error must not depend on the
	// session having been resolved, or the sentence explaining the typo is a
	// nil dereference and a Go stack trace instead.
	if e.out == nil {
		plain := plainFromEnv()
		e.out = ui.New(isTerminal(os.Stdout) && !plain, !plain, false)
	}

	var unreachable *client.UnreachableError
	var apiErr *client.Error
	var usageErr *usageError
	var buildFailed *deployFailedError
	var notSignedIn *config.NotSignedInError
	var noAdminSession *config.NoAdminSessionError
	var unboundCredential *config.AmbiguousCredentialError
	var ambiguousAlias *config.AmbiguousAliasError
	var envCredential *config.EnvCredentialError
	var refusedHere *authError
	var resultFailed *resultFailedError
	var manifestErr *config.ManifestError
	var notADir *bundle.NotADirectoryError

	var appUnreach *appUnreachableError

	var interrupted *stdinInterrupted
	switch {
	case errors.As(err, &interrupted):
		// A signal while stdin was being read: nothing had printed, and
		// nothing was sent, so the refusal is said here (BB-016, third list).
		return e.refuse(err, "failed", "interrupted", exitFailed)
	case errors.Is(err, context.Canceled):
		// Ctrl-C during a watch. The message was already printed by the thing
		// that was interrupted, because only it knows what is still running.
		return exitFailed
	case errors.As(err, &buildFailed):
		// The deploy itself failed. Already narrated in full — phase, step,
		// reason, and under --json a whole result object; a second sentence
		// here would only repeat it.
		return exitFailed
	case errors.As(err, &resultFailed):
		// The answer arrived, and it was a failure. Under --json the result
		// document already says so in full — the deploy's exception, for the
		// deploy's reason — so this is the sentence alone.
		e.out.Error(err)
		return exitFailed
	case errors.As(err, &usageErr):
		if usageErr.from != nil {
			e.out.Error(err)
			e.installHint(usageErr.from)
			return e.errorDocument(err, "usage", orDefault(usageErr.code, "invalid_usage"), exitUsage)
		}
		// The document carries a hint (ownHint). On a terminal stderr stays
		// the one line a typo deserves; a plain run's reader is a program,
		// which gets the hint line an install's refusal prints (BB-093).
		e.out.Error(err)
		if !e.out.Rich() {
			if h := e.ownHint(err, orDefault(usageErr.code, "invalid_usage")); h != "" &&
				!strings.Contains(strings.ToLower(err.Error()), strings.ToLower(h)) {
				e.out.Hint("%s", sentenceCase(h))
			}
		}
		return e.errorDocument(err, "usage", orDefault(usageErr.code, "invalid_usage"), exitUsage)
	case errors.As(err, &manifestErr):
		// A file the developer wrote and this client cannot read: nothing was
		// sent, so nothing failed on the platform.
		return e.refuse(err, "usage", "manifest_invalid", exitUsage)
	case errors.As(err, &notADir):
		return e.refuse(err, "usage", "no_such_directory", exitUsage)
	case errors.As(err, &unboundCredential), errors.As(err, &ambiguousAlias),
		errors.As(err, &envCredential):
		// Usage rather than auth, and the distinction is the point: no install
		// refused anything, because nothing was sent to one. Both refusals are
		// statements about how the command was invoked, and both name the
		// one-line fix — and each carries a code, as every exit-2 refusal does.
		var coded interface{ Code() string }
		code := ""
		if errors.As(err, &coded) {
			code = coded.Code()
		}
		return e.refuse(err, "usage", code, exitUsage)
	case errors.As(err, &notSignedIn), errors.As(err, &noAdminSession), errors.Is(err, config.ErrNoInstall), errors.As(err, &refusedHere):
		code, kind := "", "auth"
		// Each carries a code, as every refusal does (BB-037).
		switch {
		case notSignedIn != nil:
			code = "not_signed_in"
		case noAdminSession != nil && !noAdminSession.Expired.IsZero():
			code = "session_expired"
		case noAdminSession != nil:
			code = "no_session"
		case errors.Is(err, config.ErrNoInstall):
			code = "no_install"
		}
		if refusedHere != nil {
			code = refusedHere.code
			if refusedHere.refused {
				kind = "refused"
			}
			if refusedHere.from != nil {
				e.out.Error(err)
				e.installHint(refusedHere.from)
				return e.errorDocument(err, kind, code, exitAuth)
			}
			if refusedHere.hint != "" {
				e.out.Error(err)
				e.out.Hint("%s", sentenceCase(refusedHere.hint))
				return e.errorDocument(err, kind, code, exitAuth)
			}
		}
		return e.refuse(err, kind, code, exitAuth)
	case errors.As(err, &appUnreach):
		// A fetch that reached nothing — the app's hostname not resolving,
		// a connection refused (BB-101): exit 4, coded, with its next step.
		e.out.Error(err)
		if !strings.Contains(appUnreach.msg, appUnreach.hint) {
			e.out.Hint("%s", sentenceCase(appUnreach.hint))
		}
		return e.errorDocument(err, "unreachable", appUnreach.code, exitUnreachable)
	case errors.As(err, &unreachable):
		return e.refuse(err, "unreachable", "unreachable", exitUnreachable)
	case errors.As(err, &apiErr) && (apiErr.Code == "route_not_found" || apiErr.Code == "method_not_allowed"):
		// **No such route is a usage error, not a failure.** The install
		// resolved nothing, so nothing was tried and nothing can be retried;
		// exit 1 would say the platform did the thing and it went wrong, and
		// exit 3 — where an unresolvable path used to land, because the
		// credential guard answered before the router could — tells an
		// unattended caller to mint a key or fetch an administrator over a
		// typo. Two causes and both are the caller's to fix: a mistyped path,
		// or a verb this binary has and the install is too old to serve. A
		// method the route does not take (405) is the same mistake one word
		// over, and its hint names the method that would have worked.
		e.out.Error(err)
		e.installHint(apiErr)
		return e.errorDocument(err, "usage", apiErr.Code, exitUsage)
	case errors.As(err, &apiErr):
		// **A locked app is not a failed build.** Nothing was built: an
		// administrator, or this install's own dependency policy, is standing
		// in the way, and it will stand in the way of the identical retry
		// forever. It arrives as 409 — the caller is usually allowed to do this
		// and the app's state is what refuses — so it used to fall through to
		// exit 1, which is the code that means "read the build log" of a build
		// that does not exist.
		if apiErr.Code == "app_locked" {
			e.out.Error(err)
			if !e.installHint(apiErr) {
				e.out.Hint("Nothing was built. Retrying will refuse " +
					"identically until the lock is lifted.")
			}
			return e.errorDocument(err, "locked", apiErr.Code, exitUsage)
		}
		// **A session refused is a session that ended**, not a missing
		// credential: the install revokes a person's sessions when their role
		// or groups change, and expires them on its own clock, while the copy
		// stored here still looks current. The install's hint ("send a
		// credential … or run `toft login`") names neither the session nor the
		// sign-in that replaces it, so this one does — in the text and under
		// --json alike.
		// The admin-only routes answer a session they no longer admit with the
		// same 404 they give a non-administrator, hiding that they exist; ask
		// the install which one this was, and say a session that ended as one
		// (drills 2026-11-02 O4, 2026-11-03).
		if e.sessionAlive != nil && apiErr.Status == http.StatusNotFound && apiErr.Code == "not_found" && !e.sessionAlive() {
			apiErr.Status, apiErr.Code = http.StatusUnauthorized, "unauthorized"
			apiErr.Message = "the stored session no longer works, so the install hid this route from it (" + apiErr.Message + ")"
			apiErr.Retryable = nil
		}
		if e.sessionInstall != "" && apiErr.Status == http.StatusUnauthorized {
			apiErr.Hint = fmt.Sprintf("the stored session was refused — it expired, or was revoked "+
				"(a change to your role or groups ends every session). Sign in again: "+
				"`toft login --password-stdin %s`", e.sessionInstall)
		}
		e.out.Error(err)
		if e.installHint(apiErr) {
			if apiErr.Unauthorized() {
				return e.errorDocument(err, "auth", apiErr.Code, exitAuth)
			}
			// A hint is the install's next step, not a different exit: a
			// `name_taken` carries one and is still exit 2.
			kind, exit := classifyRefusal(apiErr)
			return e.errorDocument(err, kind, apiErr.Code, exit)
		}
		if apiErr.Unauthorized() {
			// The install decides an expiry, never the client — there is no
			// client-side check gating the request. When it says so, the fix is
			// one command and worth naming, because this is the code a pipeline
			// hits at whatever hour it runs.
			if apiErr.Code == "key_expired" {
				e.out.Hint("That key has expired — run `toft login` to replace it.")
			}
			// A read-scoped key, which `toft login` never creates: this one was
			// created in the browser and handed to `toft login --token-stdin`.
			// Worth a line because the install's sentence names the remedy in
			// its own terms and the person reading a pipeline log needs to know
			// the credential is doing exactly what it was created to do.
			if apiErr.Code == "key_read_scope" {
				e.out.Hint("That key can read and not write. `toft login` " +
					"creates a full-scope key; a read-scoped one has to be replaced, not widened.")
			}
			// The only denial the authorizer produces for a system role, and
			// it has exactly one cause: publishing needs the `publisher`
			// role and this account is a viewer. The install's own half is
			// the bare word "forbidden" — correct, because a refusal should
			// not narrate the policy that produced it to whoever is
			// probing, but it reads as a broken command rather than as an
			// account that was never given the rung. The remedy is a
			// person, so the line has to say so; there is no flag and no
			// retry that reaches it.
			if apiErr.Code == "deny_system_role" {
				e.out.Hint("This account is a viewer on this install, and creating or deploying " +
					"an app needs the publisher role.\n" +
					"An administrator grants that — nothing here succeeds on a retry.")
			}
			// A commercial refusal, and it shares exit 3 with a permission
			// problem — deliberately, because 402 would fall out of
			// `Unauthorized()` and report it as a failed build. The code and
			// the install's own sentence are the distinguisher, and this line
			// is what stops a pipeline log reading as "you are not allowed".
			if apiErr.Code == "tier_limit" {
				e.out.Hint("That is a license limit on the install, not a permission problem — " +
					"nothing you already deployed has stopped.")
			}
			return e.errorDocument(err, "auth", apiErr.Code, exitAuth)
		}
		kind, exit := classifyRefusal(apiErr)
		return e.errorDocument(err, kind, apiErr.Code, exit)
	}
	e.out.Error(err)
	var ended *runEndedError
	if errors.As(err, &ended) {
		return e.errorDocument(err, "failed", ended.status, exitFailed)
	}
	// Some of a batch failed: each item carries its own code in the list,
	// and the batch's final document carries this one (M2, 2026-11-04).
	var partial *partialError
	if errors.As(err, &partial) {
		return e.errorDocument(err, "failed", "partial_failure", exitFailed)
	}
	var coded *failedError
	if errors.As(err, &coded) {
		if coded.hint != "" && !strings.Contains(coded.msg, coded.hint) {
			e.out.Hint("%s", sentenceCase(coded.hint))
		}
		return e.errorDocument(err, "failed", coded.code, exitFailed)
	}
	return e.errorDocument(err, "failed", "", exitFailed)
}

// failedError is an exit-1 failure this client names itself — a wait that ran
// out (`wait_timeout`), an app that did not answer in time (`timeout`) — with
// a code, and whether the identical retry may succeed.
//
// hint is the next step, when the sentence alone does not end with it — the
// refusal document's `hint` (BB-021, third 2026-11-03 list).
type failedError struct {
	msg, code, hint string
	retryable       bool
}

func (e *failedError) Error() string { return e.msg }

// classifyRefusal is the one mapping from an install's refusal onto the exit
// contract, for every verb at once. Before it, each verb that wanted the right
// code wrapped its own call (`noSuchName`), and every verb that did not
// answered 1 — "the platform did the work and it failed" — for a deployment id
// that named nothing, or a name already taken.
// Nothing ran in any of them, so 1 sent a reader to a log that does not exist.
//
//   - 404 and 400: the request named nothing, or said something the install
//     cannot read. Usage, 2 — the caller changes the invocation.
//   - 409 `name_taken` or `hostname_taken`: the name is the caller's to
//     change, like an already-linked directory. 2.
//   - 409 that is the install's to clear — `queue_full`, `already_running`,
//     `deploy_queued`, or
//     anything the envelope calls retryable: the outcome a pipeline retries,
//     and the one a run that ran and failed already exits with. 1.
//   - any other 409: the app's state refuses — the serving deployment, a batch app asked for a service. The identical retry refuses
//     forever and something has to change first. 3, the refusal code.
//
// Everything else stays 1. `app_locked` keeps its own 2 above it.
//
// The jobs codes are named before any status, because their exits are the
// spec's and a status is only how one install happened to send them:
// `jobs_unavailable` is an administrator's decision (3) whether it arrives as
// 422 or 409, `run_still_open` is the run's state refusing (3), `queue_full`
// is the one a later try clears (1), and a parameter the job refuses or a job
// it does not declare, or params over the 64 KB cap (params_too_large, a
// 413), is the invocation to fix (2).
func classifyRefusal(apiErr *client.Error) (kind string, exit int) {
	switch apiErr.Code {
	case "jobs_unavailable", "run_still_open":
		return "refused", exitAuth
	case "queue_full":
		return "failed", exitFailed
	case "param_required", "param_type", "param_choice", "param_unknown", "no_such_job", "params_too_large":
		return "usage", exitUsage
	case "owner_group_empty", "owner_group_no_eligible_member", "owner_group_mapped",
		"owner_not_found", "owner_is_viewer", "owner_user_locked":
		// The install's owner refusals (422) are the --owner/--owner-group the
		// caller named, to change as owner_group_required and an unknown group
		// are when this client catches them first: 2, one code for the family.
		return "usage", exitUsage
	}
	switch apiErr.Status {
	case http.StatusNotFound, http.StatusBadRequest:
		return "usage", exitUsage
	case http.StatusConflict:
		switch {
		case apiErr.Code == "name_taken", apiErr.Code == "hostname_taken":
			// A taken name or hostname is the caller's to change (L3).
			return "usage", exitUsage
		case apiErr.Code == "already_running", apiErr.Code == "deploy_queued", apiErr.ShouldRetry():
			return "failed", exitFailed
		}
		return "refused", exitAuth
	}
	return "failed", exitFailed
}

// refuse says why on stderr, writes the machine-readable half on stdout when
// this run asked for it, and returns the exit code.
//
// The sentence goes to stderr in **both** modes rather than being replaced by
// the document. stdout is the answer and stderr is the narration everywhere
// else in this client, and a person who pipes stdout to `jq` still wants to see
// what went wrong on their terminal.
func (e *env) refuse(err error, kind, code string, exit int) int {
	e.out.Error(err)
	return e.errorDocument(err, kind, code, exit)
}

// errorDocument writes the one object a `--json` caller parses, and nothing at
// all otherwise.
//
// `kind` is this client's own classification and is always present; `code` is
// the install's, and is omitted when there is none rather than sent empty — so
// a consumer testing for the key learns "the install did not name one" instead
// of reading "" as a category. `exit` is in the document as well as in the exit
// status, because a caller that captured output and lost the status should not
// have to guess.
//
// `retryable` is always present, and `hint` whenever the install sent one. Both
// are the install's when the refusal was the install's; a refusal this client
// made itself is retryable only when the install could not be reached, because
// every other one is about how the command was invoked and refuses again.
func (e *env) errorDocument(err error, kind, code string, exit int) int {
	if !e.json {
		return exit
	}
	doc := map[string]any{"error": err.Error(), "kind": kind, "exit": exit,
		"retryable": kind == "unreachable"}
	if code != "" {
		doc["code"] = code
	}
	var unreachable *client.UnreachableError
	if errors.As(err, &unreachable) {
		doc["retryable"] = unreachable.ShouldRetry()
	}
	var failed *failedError
	if errors.As(err, &failed) {
		doc["retryable"] = failed.retryable
		if failed.hint != "" {
			doc["hint"] = failed.hint
		}
	}
	var appUnreach *appUnreachableError
	if errors.As(err, &appUnreach) {
		doc["retryable"] = appUnreach.retryable
		doc["hint"] = appUnreach.hint
	}
	var ended *runEndedError
	if errors.As(err, &ended) && ended.hint != "" {
		doc["hint"] = ended.hint
	}
	var interrupted *stdinInterrupted
	if errors.As(err, &interrupted) {
		doc["hint"] = "nothing was sent; run it again with its input ready on stdin"
	}
	var apiErr *client.Error
	if errors.As(err, &apiErr) {
		doc["retryable"] = apiErr.ShouldRetry()
		var usage *usageError
		if apiErr.Hint != "" {
			doc["hint"] = apiErr.Hint
		} else if h, ok := installFallbackHints[apiErr.Code]; ok {
			doc["hint"] = h
		} else if errors.As(err, &usage) && usage.hint != "" {
			// This client's own next step for an install refusal that had
			// none — `toft job cancel` on an unknown run (BB-082).
			doc["hint"] = usage.hint
		}
		// A deploy refused at the upload has a reason like one that failed
		// later, and the skill says to read `reason` first (docs/bugs
		// 2026-10-28 #3).
		if apiErr.Reason != "" {
			doc["reason"] = apiErr.Reason
		}
		// A full queue's figures, as the install's body has them: how many
		// runs wait, the cap, and when to try again (2026-11-04 drift list).
		if apiErr.Code == "queue_full" && len(apiErr.Body) > 0 {
			var figures map[string]json.RawMessage
			if json.Unmarshal(apiErr.Body, &figures) == nil {
				for _, k := range []string{"in_flight", "limit", "retry_after"} {
					if v, ok := figures[k]; ok {
						doc[k] = v
					}
				}
			}
		}
		// The failed deployment the refusal recorded, when the install
		// named it (BB-016, third 2026-11-03 list).
		if apiErr.DeploymentID != "" {
			doc["deployment_id"] = apiErr.DeploymentID
		}
	}
	if _, has := doc["hint"]; !has && apiErr == nil {
		if h := e.ownHint(err, code); h != "" {
			doc["hint"] = h
		}
	}
	json.NewEncoder(e.out.Stdout).Encode(doc)
	return exit
}

// usageHints is the next step for the refusals this client makes whose
// sentence names none (BB-042, fourth 2026-11-03 list). %s is the command.
var usageHints = map[string]string{
	"invalid_slug":     "a slug is lowercase letters, digits and dashes — `toft apps` lists the ones you can see",
	"bad_flag":         "`toft %s --help` lists its flags and what each takes",
	"unknown_flag":     "`toft %s --help` lists its flags",
	"invalid_duration": "write a duration as a number and a unit: 30s, 15m, 2h",
	"no_app":           "pass --app <slug>, or run `toft link <slug>` here — `toft apps` lists them",
	"not_signed_in":    "`toft login <install-url>` signs in, or set TOFT_URL and TOFT_TOKEN",
	"no_install":       "`toft login <install-url>` signs in, or set TOFT_URL and TOFT_TOKEN",
	"no_session":       "`toft login --password-stdin <install-url>` starts an administrator session",
	"session_expired":  "`toft login --password-stdin <install-url>` starts a new session",
	"invalid_key":      "check TOFT_TOKEN (or TOFT_TOKEN_FILE), or `toft login <install-url>` for a new key",
	"unreachable":      "`toft login --status` shows the address in use; retry once it answers",
	// Not --help: each names the step that gets past it (BB-053, fifth
	// 2026-11-03 list).
	"confirmation_required": "pass --yes to go ahead without being asked",
	"no_credential":         "`toft login <install-url>` stores one, or set TOFT_TOKEN — `toft login --status` shows what is stored",
}

// installFallbackHints is the next step for an install's refusal that arrived
// without one — an install older than its hint, or a code whose envelope
// never carried one. The install's own hint always wins (BB-053, fifth
// 2026-11-03 list).
var installFallbackHints = map[string]string{
	"no_service":           "a [batch] app has no service to start, stop or restart — its work is its jobs: `toft job list` shows them, `toft job run <job>` runs one now",
	"sdk_language_unknown": "pass --lang with one of the languages the install names, like `toft sdk vendor --lang python`",
	// An install from before it hinted this one answers a viewer's deploy,
	// rollback or visibility change with the bare word (black-box BB-1).
	"deny_no_action": `your role on this app does not allow that — its owner can make you an editor: ` +
		"`toft api POST apps/<id>/access-requests --data '{\"role\":\"editor\"}'` asks them " +
		"(an account with the viewer role needs the publisher role first: `toft api POST me/promotion-requests`)",
	// Deleting an app, changing who can see it and managing its grants are
	// the owner's alone: no role a caller could request helps (BB-32).
	"deny_owner_only": "only the app's owner, or an administrator, can delete it, change who can see it or manage who has access — ask them; no role you could request allows it",
}

// ownHint is the hint for a refusal this client made itself: the one it was
// built with, usageHints' line for its code, the next step its sentence ends
// on (the part after " — "), or — for a usage refusal — the command's --help.
func (e *env) ownHint(err error, code string) string {
	var usage *usageError
	if errors.As(err, &usage) && usage.hint != "" {
		return usage.hint
	}
	var failed *failedError
	if errors.As(err, &failed) && failed.hint != "" {
		return failed.hint
	}
	var auth *authError
	if errors.As(err, &auth) && auth.hint != "" {
		return auth.hint
	}
	// A host name that does not resolve is not retryable, so its hint must not
	// say to retry: it says to check the address (F6, 2026-11-03 evening drills).
	var unreachable *client.UnreachableError
	if code == "unreachable" && errors.As(err, &unreachable) && !unreachable.ShouldRetry() {
		return "the install's host name does not resolve — check the address `toft login --status` shows, or TOFT_URL"
	}
	command := strings.TrimSpace(e.command)
	if h, ok := usageHints[code]; ok {
		if strings.Contains(h, "%s") {
			if command == "" {
				return "`toft --help` lists the commands; each has its own --help"
			}
			return fmt.Sprintf(h, command)
		}
		return h
	}
	msg := err.Error()
	if i := strings.Index(msg, " — "); i >= 0 {
		if tail := strings.TrimSpace(msg[i+len(" — "):]); tail != "" {
			return tail
		}
	}
	if usage != nil {
		if command == "" {
			return "`toft --help` lists the commands; each has its own --help"
		}
		return fmt.Sprintf("`toft %s --help` shows how it is written", command)
	}
	return ""
}

// installHint prints the install's next step under its sentence and reports
// whether there was one. **The install's wording wins over this client's**: the
// lines report() keeps for particular codes are for an install older than the
// envelope, and printing both would be two wordings of one rule.
func (e *env) installHint(apiErr *client.Error) bool {
	hint := apiErr.Hint
	if hint == "" {
		hint = installFallbackHints[apiErr.Code]
	}
	if hint == "" {
		return false
	}
	// Some refusals already end with their next step; printing it again under
	// the sentence is the same words twice.
	if strings.Contains(strings.ToLower(apiErr.Message), strings.ToLower(hint)) {
		return true
	}
	e.out.Hint("%s", sentenceCase(hint))
	return true
}

// sentenceCase capitalises a hint that is prose. One that starts with a
// command to paste — "toft job wait 6fec1fe2" — is left as written:
// capitalised, it is a command that does not run.
func sentenceCase(hint string) string {
	if strings.HasPrefix(hint, "toft ") {
		return hint
	}
	return strings.ToUpper(hint[:1]) + hint[1:]
}

// authError is a refusal this client makes on the install's behalf, from
// something the install already said — a credential shown key names and not
// their values cannot be the one that rewrites them. Exit 3, like the install's
// own refusals of the same kind.
//
// `refused` is a refusal of state rather than of the credential — a store's
// level not reaching a replace or a remove — and reports `kind: refused`, as
// the install's own 409s at exit 3 do.
type authError struct {
	msg, code string
	refused   bool
	// from is the install's refusal this one restates, when there was one —
	// its hint is the install's to give (drills 2026-11-02: an
	// outside_mapped_folders refusal lost it).
	from *client.Error
	// hint, when set, is this refusal's own next step, ahead of usageHints'
	// line for its code — which names TOFT_TOKEN, wrong for a key read from
	// stdin (BB-068).
	hint string
}

func (e *authError) Error() string { return e.msg }

func (e *authError) Unwrap() error {
	if e.from == nil {
		return nil
	}
	return e.from
}

func authf(format string, args ...any) error {
	return &authError{msg: fmt.Sprintf(format, args...)}
}

// authCodef is authf with a code for the --json document — and the code is
// also what lets a batch whose every item was this refusal be that refusal.
func authCodef(code, format string, args ...any) error {
	return &authError{msg: fmt.Sprintf(format, args...), code: code}
}

// refusedCodef is a coded exit-3 refusal of state, `kind: refused`.
func refusedCodef(code, format string, args ...any) error {
	return &authError{msg: fmt.Sprintf(format, args...), code: code, refused: true}
}

// refusalCode is the code a --json document would carry for err: the
// install's, or this client's own — what a batch's per-item list names.
func refusalCode(err error) string {
	var apiErr *client.Error
	var own *authError
	var usage *usageError
	var failed *failedError
	switch {
	case errors.As(err, &usage) && usage.code != "":
		return usage.code
	case errors.As(err, &failed) && failed.code != "":
		return failed.code
	case errors.As(err, &own):
		return own.code
	case errors.As(err, &apiErr):
		return apiErr.Code
	}
	return ""
}

// usageError is a refusal about how the command was invoked rather than about
// what the platform did. Exit 2, and it names the flag that would have worked.
//
// `code` is the machine-readable half under --json, when there is one worth
// naming: the install's own code when the refusal began as the install's (a
// name it could not resolve), or this client's for a refusal it made itself
// (`manifest_invalid`, `no_such_directory`). Empty is "usage" alone.
type usageError struct {
	msg  string
	code string
	// hint is the next step, when the sentence does not end with one; a
	// refusal without it gets usageHints' line, or its own --help (BB-042).
	hint string
	// from is the install's refusal this one restates, when there was one:
	// its hint and retryable flag are the install's to give, and a usage
	// error that dropped them was the one refusal in the client without them.
	from *client.Error
}

func (e *usageError) Error() string { return e.msg }

func (e *usageError) Unwrap() error {
	if e.from == nil {
		return nil
	}
	return e.from
}

// usageCodef is a usage refusal and its code for the --json document. There is
// no uncoded form: every usage refusal carries a `code` (BB-006).
func usageCodef(code, format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...), code: code}
}

// unknownSubcommand refuses what stands where a subcommand goes. A flag there
// is a subcommand left out, not a subcommand named "--json" (BB-086).
// The hint carries every word that followed, so `deployments --app x --json`
// suggests `deployments list --app x --json`, slug and all (BB-093).
func unknownSubcommand(command string, args []string, choices string) error {
	got := args[0]
	first := strings.SplitN(strings.ReplaceAll(choices, " or ", ", "), ", ", 2)[0]
	if strings.HasPrefix(got, "-") {
		return usageHinted("missing_argument", fmt.Sprintf("toft %s %s %s", command, first, strings.Join(args, " ")),
			"toft %s needs a subcommand before %s — %s", command, got, choices)
	}
	// The hint names the command's own --help: the generic one would name
	// "toft <command> <got> --help", a command that does not exist (BB-083).
	return usageHinted("unknown_command", fmt.Sprintf("`toft %s --help` lists its subcommands", command),
		"toft %s has no subcommand %q — try %s", command, got, choices)
}

// usageHinted is usageCodef with the next step stated apart from the sentence.
func usageHinted(code, hint, format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...), code: code, hint: hint}
}

// deployFailedError is the platform doing the thing and it failing. Carried as
// a distinct type only so `report` can tell it from a client-side problem — the
// narration has already happened.
type deployFailedError struct{ phase, step, reason string }

func (e *deployFailedError) Error() string {
	if e.step == "" {
		return "the deploy failed"
	}
	return fmt.Sprintf("the deploy failed during %s: %s", e.phase, e.step)
}

func usage(w *os.File) {
	ui.Help(w, `toft — the Toft command-line client

Usage:
  toft <command> [arguments]

Commands:
  login        Sign this machine in to a Toft install
  link         Record which app this directory deploys to
  create       Create an app and deploy this tree into it
  deploy       Bundle this directory and deploy it
  check        What a deploy of this directory would decide, without one
  status       What is serving, since when, and from what
  fetch        Request a path on the deployed app, as you
  deps         What the dependency policy says about the app
  logs         The app's runtime log, or one build's
  events       What has gone wrong with this app
  job          Jobs: your app's scheduled and background work
  redeploy     Build the app again from its own source
  rollback     Make an earlier deployment current again
  restart      Restart the app, starting it if it is stopped
  start        Start the app; one already running is left alone
  stop         Stop the app
  env          The app's environment variables
  visibility   Who may open the app
  hostname     The app's own hostname (administrators)
  deployments  The app's deployment history, and pruning it
  delete       Delete an app
  apps         The apps you can see
  top          The apps you can see, kept current on a terminal
  store        Files in a durable store, from outside the platform
  storage      The storage you can attach, and what an app has attached
  sdk          The app SDK this install carries, into this directory
  api          Call this install's HTTP API with the stored credential
  whoami       Who you are here, and what this install will let you do
  skill        Print an agent skill written for this binary

Common flags:
  --app <slug>        Which app. Otherwise TOFT_APP, then toft.toml.
  --install <url>     Which install. Otherwise the default from toft login.
  --json              Machine-readable output on stdout: **one JSON document per
                      line**, not one per command. A watched deploy streams a
                      frame per phase, and a refusal adds a final error object
                      (a failed deploy's last frame is itself the coded
                      result) — so read it a line at a time, as jq does; a
                      parser expecting one document will stop at the second.
  -q, --quiet         No narration. The answer still goes to stdout.
  --yes               Answer confirmations. Required when nothing can be asked.
  --no-color          Never colour. NO_COLOR and TERM=dumb do the same.
  --plain             The reader is a program: plain lines, no colour, and
                      every question refuses naming its flag, even in a pty.
                      TOFT_PLAIN=1 does the same; --plain=false undoes it.
  --session           Act through the session toft login --password-stdin
                      stored, not the key — for the routes a key cannot reach.

Run "toft <command> --help" for a command's own flags.

Exit codes:
  0  it worked
  1  the platform did the thing and it failed
  2  usage or configuration
  3  not authenticated, or refused
  4  could not reach the install
`)
}

// globalFlags are the ones every command accepts. Parsed by each command's own
// FlagSet so that `toft deploy --json` and `toft apps --json` cannot disagree about
// what --json means.
type globalFlags struct {
	app     string
	install string
	json    bool
	quiet   bool
	yes     bool
	noColor bool
	// plain is `--plain`: this run's reader is a program. Unset is the
	// environment's answer (TOFT_PLAIN); set either way, it wins (plainRun).
	plain optionalBool
	// session presents the signed-in session `toft login --password-stdin`
	// stored, instead of the key — the administrator's half of a run that
	// would otherwise need a browser (grants, locks, settings, integrations).
	session bool
}

func (g *globalFlags) register(fs *flagSet) {
	fs.StringVar(&g.app, "app", "", "the app slug")
	fs.StringVar(&g.install, "install", "", "the install URL or alias")
	fs.BoolVar(&g.json, "json", false, "machine-readable output on stdout")
	fs.BoolVar(&g.quiet, "quiet", false, "no narration")
	fs.BoolVar(&g.quiet, "q", false, "no narration")
	fs.BoolVar(&g.yes, "yes", false, "answer confirmations")
	fs.BoolVar(&g.noColor, "no-color", false, "never colour output")
	fs.Var(&g.plain, "plain", "the reader is a program: piped output, nothing asked")
	fs.BoolVar(&g.session, "session", false, "act through the stored password session, not the key")
}

// env is everything a command needs that is not its own flags.
type env struct {
	out    *ui.Out
	ctx    context.Context
	cancel context.CancelFunc
	// json is this run having asked for machine-readable output, read off the
	// raw arguments rather than off the parsed flag set.
	//
	// **Because the case that most needs it is the case where parsing failed.**
	// A caller that passed a bad flag, or no credential, never reaches its
	// command's FlagSet, and that refusal is exactly the one a program has to
	// understand. `--json` means one thing on every command (globalFlags exists
	// so it cannot mean two), so reading it here says nothing the parsed value
	// would not have — and setup() overwrites it with the parsed one as soon as
	// there is one.
	json bool
	// interrupted is closed on the first Ctrl-C, so a watch can say what is
	// still running on the platform rather than dying silently.
	interrupted chan struct{}
	// interruptOnce guards closing it: a signal and a Ctrl-C key in an input
	// program (which raw mode delivers as a key) are two ways to one interrupt.
	interruptOnce sync.Once
	dir           string
	// verify is `--verify <path>` on the commands that watch a deploy: the
	// path to fetch once the platform says it succeeded. Empty asks nothing.
	verify       string
	verifyWithin time.Duration
	// servingAfter is the deployment a successful watch should name, when it
	// is not the one being watched. A rollback is watched through a transient
	// row the platform deletes on success; the release then serving is the
	// one it restored (`source_deploy_id`), and that is the id worth printing.
	servingAfter string
	// rejoined is `toft deploy --watch <id>`: the watch began after the
	// deploy, so this client's own clock measured none of it, and the
	// duration is the row's (recordedDuration).
	rejoined bool
	// bundleSecrets are the secret-looking files the uploaded bundle carries,
	// for the --json result's warnings — the text half says `including`.
	bundleSecrets []string
	// bundleEnvFiles are the .env files the bundle left out, said in the
	// result as toft check says them (L2).
	bundleEnvFiles []string
	// bundleWorkspaceLeftOut are the go.work directories the bundle shipped
	// nothing of (BB-028).
	bundleWorkspaceLeftOut []string
	// plainLogs is `toft logs` writing somewhere that is not a terminal, or
	// asked for no colour: the app's escape sequences are removed (logText).
	plainLogs bool
	// stdin and stdinTerminal are where a question is answered from, and
	// whether a person is there to answer it. Nil is os.Stdin and its own
	// answer; a test hands in both, because a prompt it cannot drive is a
	// prompt nothing checks.
	stdin         io.Reader
	stdinTerminal func() bool
	// command is the command words this run was given (`env list`), for the
	// hint a usage refusal with no better one carries: its --help.
	command string
	// logSink is where `toft logs` writes the app's lines while a footer is
	// drawn under them (above it, through the program); nil is stdout.
	logSink io.Writer
	// sessionInstall is the install whose stored password session this run
	// presented (`--session`), or "" when it presented the key. A 401 then is
	// that session ended on the install's side, and report() names the way
	// back to it rather than the install's generic credential hint.
	sessionInstall string
	// sessionAlive asks the install whether the presented session still
	// works, set alongside sessionInstall. An administrator-only route hides
	// itself with a 404 from anyone it does not admit — a revoked session
	// included, by design (docs/invariants.md) — so report() asks this before
	// letting a session's not_found read as a missing thing.
	sessionAlive func() bool
}

// logOut is where the app's log lines go: stdout, or above the footer.
func (e *env) logOut() io.Writer {
	if e.logSink != nil {
		return e.logSink
	}
	return e.out.Stdout
}

func newEnv() (*env, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{ctx: ctx, cancel: cancel, dir: dir, interrupted: make(chan struct{})}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			e.interrupt()
		case <-ctx.Done():
			return
		}
		// A second signal is the person insisting: the first cancelled the
		// run's context, which every request now carries, but a call that
		// cannot be cancelled must not make Ctrl-C a no-op twice (code
		// review 2026-11-11, L-1).
		<-sig
		fmt.Fprintln(os.Stderr, "toft: interrupted again; exiting")
		os.Exit(130)
	}()
	return e, nil
}

func (e *env) stop() { e.cancel() }

// interrupt is the run being interrupted: what Ctrl-C does, from a signal or
// from a key an input program read.
func (e *env) interrupt() {
	e.interruptOnce.Do(func() {
		if e.interrupted != nil {
			close(e.interrupted)
		}
		if e.cancel != nil {
			e.cancel()
		}
	})
}

// setup finishes building the environment from the flags a command parsed, and
// resolves the session. Every command that talks to an install starts here.
func (e *env) setup(g *globalFlags) (*session, error) {
	// Built once. In an ordinary run this is where the writer comes from; a
	// test hands the env a writer of its own beforehand, and setup leaves it
	// alone rather than replacing it with one pointed at the real terminal.
	e.buildOut(g)
	// The parsed flag wins over run()'s scan of the raw arguments. That scan
	// exists for the refusals that happen before any command parses anything;
	// from here on there is a real answer, and two sources for one fact must
	// not be allowed to disagree.
	e.json = g.json
	// An app's log line written to a pipe or a file is the text alone, for
	// every command that prints one (`toft job run`, `job logs`), not only
	// `toft logs` — logText reads this.
	e.plainLogs = e.plainLogs || g.noColor || os.Getenv("NO_COLOR") != "" ||
		os.Getenv("TERM") == "dumb" || !writesToTerminal(e.out.Stdout)

	flags := map[config.Key]string{}
	if g.app != "" {
		flags[config.KeyApp] = g.app
	}
	if g.install != "" {
		flags[config.KeyInstall] = g.install
	}
	resolver, err := config.NewResolver(e.dir, flags)
	if err != nil {
		return nil, err
	}

	resolve := config.ResolveSession
	if g.session {
		resolve = config.ResolveAdminSession
	}
	sess, err := resolve(resolver.Install())
	if err != nil {
		return nil, err
	}

	// Two lines that belong here and nowhere else, because this is the one place
	// a session is resolved: said once per invocation rather than per request,
	// and always to stderr. The piped output of `toft apps` is a contract, and a
	// warning inside it is a parsing bug in somebody's script.
	if warning := sess.ExpiryWarning(time.Now()); warning != "" {
		if e.out.Rich() {
			e.out.Warn("%s", e.out.Dimmed(warning))
		} else {
			e.out.Dim("%s", warning)
		}
	}
	if strings.HasPrefix(sess.InstallURL, "http://") && !isLoopback(sess.InstallURL) {
		// Not a refusal: a stored credential for an http:// install is
		// somebody's existing setup, and breaking it here would be a change of
		// subject. `toft login` is where plaintext is refused.
		e.out.Dim("%s is plaintext — everything this command sends, credential included, is readable in transit.",
			sess.InstallURL)
	}

	if !sess.Cookie && sess.Token != "" && !strings.HasPrefix(sess.Token, "toft") {
		// **Not shaped like any credential an install issues** — every one
		// begins `toft` (toftu_, toftd_, the retired toft_). Sent, it was
		// one more anonymous request, and an install that answered that with
		// an empty list made a broken key in CI look like an empty install
		// (BB-033, fourth 2026-11-03 list). Exit 3, as the install's own
		// invalid_key is; nothing was sent.
		return nil, notAToftKey("the credential in use")
	}

	c := client.New(sess.InstallURL, sess.Token, "toft/"+version)
	c.Context = e.ctx
	c.Cookie = sess.Cookie
	if sess.Cookie {
		e.sessionInstall = sess.InstallURL
		e.sessionAlive = func() bool {
			var cErr *client.Error
			err := c.Do(http.MethodGet, "/api/v1/auth/me", nil, nil)
			return !(errors.As(err, &cErr) && cErr.Status == http.StatusUnauthorized)
		}
		c.OnRenew = func(token string, expires time.Time) { config.RenewAdminSession(sess.InstallURL, token, expires) }
	}
	return &session{Session: sess, client: c, resolver: resolver, flags: g}, nil
}

// session is one command's resolved world: which install, as whom, with which
// layers still to consult for the app.
type session struct {
	*config.Session
	client   *client.Client
	resolver *config.Resolver
	flags    *globalFlags
	// slugTyped is a slug this command line named as an argument (toft
	// delete <slug>), so a refusal never says it was read from a file.
	slugTyped bool

	// The install's own answer about itself, read at most once per invocation
	// by session.config(). cfgRead rather than a nil check: a probe that failed
	// is a fact, and re-asking on every line that prints a URL would turn one
	// blip into four.
	cfg     installConfig
	cfgRead bool
}

// app resolves the app slug this directory is about, or refuses with the
// command that would fix it.
//
// `toft deploy` never creates an app. Creating one takes a publish permission and
// claims a slug that becomes a URL in the estate's namespace; it is not a thing
// to do as a side effect of a typo in a directory name.
func (s *session) app() (config.Value, error) {
	v := s.resolver.App(s.InstallURL)
	if !v.Found() {
		return v, usageCodef("no_app", "no app is linked in this directory — run `toft link <slug>`, "+
			"or pass --app <slug>")
	}
	return v, nil
}

// unattended reports whether this run may not ask a question.
//
// **A mode the CLI detects, not a flag the user remembers.** The failure this
// prevents is the one that matters: a CI job that hangs for an hour on a
// confirmation nobody can see, and is killed by a timeout that reports nothing
// useful.
func unattended(e *env, g *globalFlags) bool {
	if g.yes || plainRun(g) {
		return true
	}
	return os.Getenv("CI") != "" || !e.stdinIsTerminal()
}

// plainRun is whether this run's reader is a program — `--plain`, or
// TOFT_PLAIN when the flag was not given.
//
// **The caller's word, for where detection cannot tell.** An agent driving
// `toft` through a pty looks exactly like a person to every isatty test, and
// would get the drawn tier, the live tree and a question it cannot answer. Set,
// the run is a piped run: plain lines on both streams, no colour, and every
// question refuses naming the flag that would have answered it. The flag beats
// the environment, so `--plain=false` gives a person in a shell that exports it
// the terminal back. Not a `toft.toml` key: whether the reader is a program is
// a fact about the caller, and a cloned tree must not decide it for whoever
// runs it.
func plainRun(g *globalFlags) bool {
	if g != nil && g.plain.set {
		return g.plain.value
	}
	return plainFromEnv()
}

// plainFromEnv is TOFT_PLAIN, read the NO_COLOR way: any value but empty and
// "0" is set.
func plainFromEnv() bool {
	v := os.Getenv("TOFT_PLAIN")
	return v != "" && v != "0"
}

// optionalBool is a boolean flag that also knows whether it was given, which a
// flag the environment can answer needs: unset defers, false overrides.
type optionalBool struct{ set, value bool }

func (b *optionalBool) String() string {
	if b == nil {
		return "false"
	}
	return strconv.FormatBool(b.value)
}

func (b *optionalBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	b.set, b.value = true, v
	return nil
}

func (b *optionalBool) IsBoolFlag() bool { return true }

// buildOut gives the env its writer, once, from the flags a command parsed: a
// test hands the env a writer of its own beforehand, and that one is kept. A
// plain run is made a piped one either way — on a writer this built, and on one
// a test built claiming a terminal — so `TOFT_PLAIN` cannot be a no-op for a
// writer that already exists.
func (e *env) buildOut(g *globalFlags) {
	plain := plainRun(g)
	if e.out == nil {
		e.out = ui.New(isTerminal(os.Stdout) && !plain, !g.noColor && !plain, g.quiet)
	}
	if plain {
		e.out.Interactive = false
	}
	e.out.JSON = g.json
	e.out.Quiet = e.out.Quiet || g.quiet
}

// input is whether this run may build a program that reads keys: it may draw
// live, and a person is there to press them. **The same unattended() the
// confirm asks**, so a picker, a confirm and `top`'s keys cannot reach a
// different verdict about whether anybody is there.
func input(e *env, g *globalFlags) bool {
	return e.out.Live() && !unattended(e, g)
}

// stdinIsTerminal is whether a person could be typing on stdin — the env's
// seam when a test set one, the real terminal test otherwise.
func (e *env) stdinIsTerminal() bool {
	if e.stdinTerminal != nil {
		return e.stdinTerminal()
	}
	return isTerminal(os.Stdin)
}

// input is where an answer is read from: the env's seam, or os.Stdin.
func (e *env) input() io.Reader {
	if e.stdin != nil {
		return e.stdin
	}
	return os.Stdin
}

// confirm asks, unless it cannot — in which case it refuses and names the flag
// that would have answered it. Never blocks in a non-interactive run.
func confirm(e *env, g *globalFlags, question string) error {
	if g.yes {
		return nil
	}
	if unattended(e, g) {
		reason := "stdin is not a terminal"
		switch {
		case g.plain.set && g.plain.value:
			reason = "--plain is set"
		case plainRun(g):
			reason = "TOFT_PLAIN is set"
		case os.Getenv("CI") != "":
			reason = "CI is set"
		}
		return usageCodef("confirmation_required", "refusing to %s without --yes (%s)", question, reason)
	}
	sentence := strings.ToUpper(question[:1]) + question[1:] + "?"
	// A person on every stream gets a yes/no to move between, No selected.
	// Anyone else answering on a terminal — a piped stdout, say, which still
	// asks on stderr — gets the line it always has.
	if input(e, g) {
		yes, err := e.out.Confirm(e.ctx, sentence, e.input(), e.interrupt)
		if err != nil && !errors.Is(err, ui.ErrNotAnswered) {
			return err
		}
		if yes {
			return nil
		}
		return usageCodef("cancelled", "cancelled")
	}
	e.out.Ask(sentence, "[y/N]")
	var answer string
	fmt.Fscanln(e.input(), &answer)
	if strings.EqualFold(strings.TrimSpace(answer), "y") {
		return nil
	}
	return usageCodef("cancelled", "cancelled")
}

// notAToftKey is the local refusal of a credential no install could have
// issued — every one begins `toft`. Shared by TOFT_TOKEN and `toft login
// --token-stdin` so both refuse before anything is sent (BB-033, BB-054).
func notAToftKey(what string) error {
	return &authError{code: "invalid_key", msg: what + " is not a Toft key — " +
		"every key an install issues begins `toftu_` (a person's) or `toftd_` (a deploy key); nothing was sent"}
}
