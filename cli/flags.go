package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"toft/cli/internal/ui"
)

// flagSet is Go's, with our help text.
//
// The standard library's flag package is the whole command-line framework here.
// What a framework would add — subcommand registration, generated help,
// completion — is either three lines of map lookup (dispatch), the thing we most
// want to write by hand (help), or deferred (completion). What it would cost is
// a dependency in the one binary a developer downloads and runs on their laptop.
type flagSet = flag.FlagSet

// newFlags builds a command's flag set with usage text that is ours.
//
// `flag`'s generated usage prints every flag in alphabetical order with its Go
// type, which is a listing rather than an explanation. Each command writes its
// own paragraph instead.
//
// **Nothing is written by `flag` itself.** Its output goes to io.Discard and
// `parse` below turns what it would have said into this client's own refusal,
// for two reasons that were both visible on one bad flag:
//
//   - `flag` prints "flag provided but not defined: -nope" — its own normalized
//     single-dash spelling, of a flag the caller wrote with two — and it prints
//     it with no `toft:` prefix, making it the one message on stderr that does not
//     look like this program said it. The `--json` half was corrected and the
//     human half was not, so the two channels disagreed about what the caller
//     had typed.
//   - It follows it with the whole usage text, which no other refusal here does.
//     A page of help under a one-line typo buries the line that matters.
//
// The usage text is still printed for `--help`, which is a request for it rather
// than a failure; parse re-enables the writer for exactly that case.
func newFlags(name, usage string) *flagSet {
	fs := flag.NewFlagSet("toft "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		if f, ok := fs.Output().(*os.File); ok {
			ui.Help(f, usage)
			return
		}
		fmt.Fprint(fs.Output(), usage)
	}
	return fs
}

// parse runs the flag set and turns `--help` into a clean exit rather than an
// error a caller has to special-case.
//
// **Flags and operands may come in any order** (interspersed): `toft link demo
// --local` and `toft login https://toft.corp --token-stdin` read the flag
// after the word. `flag` stops at the first non-flag word and leaves the rest
// in Args(), which silently dropped `--token-stdin` — a device flow waiting for
// a human on a machine that has none — and was then refused with "put flags
// first", the fix the caller should not have needed. interspersed moves every
// flag ahead of the operands, knowing from the flag set which flags take a
// value, so `--lines -5` is still a value and never a flag. A word after `--`
// is an operand whatever it looks like.
//
// **And it refuses an operand the command does not take.** parse is the form for
// a command with no positional arguments; parseOperands names how many a command
// reads. A word nobody reads is never harmless: `toft task edit nightly
// --enabled false` parsed `--enabled` as a bare bool (true), stopped at `false`,
// and printed success over a task that was still enabled. Every leftover is now
// either an operand the command reads or exit 2.
func parse(fs *flagSet, args []string) error { return parseOperands(fs, args, 0) }

// parseAfter is parse for a command that took its one operand off the front
// with takeOperand: none more may follow when it did, and one may when it did
// not (`toft job run --app demo nightly`).
func parseAfter(fs *flagSet, args []string, taken string) error {
	if taken != "" {
		return parseOperands(fs, args, 0)
	}
	return parseOperands(fs, args, 1)
}

// parseOperands parses and allows at most max positional operands; -1 is any
// number, for a command that counts its own and says so in its own words.
func parseOperands(fs *flagSet, args []string, max int) error {
	ordered, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		if err == flag.ErrHelp {
			// Asked for, so printed — once, here, rather than by the discarded
			// automatic call `flag` already made on the way to this error.
			fs.SetOutput(os.Stderr)
			fs.Usage()
			return errHelp
		}
		if refusal := durationRefusal(fs, err); refusal != nil {
			return refusal
		}
		code := flagValueCode(fs, err)
		if code == "" {
			// Every flag refusal carries a code under --json (BB-037): a flag
			// the command does not have, or one written wrong.
			code = "bad_flag"
			if strings.HasPrefix(err.Error(), "flag provided but not defined") {
				code = "unknown_flag"
			}
		}
		return &usageError{msg: flagWords(fs, echoAsTyped(err.Error(), args)), code: code}
	}
	if max >= 0 && fs.NArg() > max {
		extra := fs.Arg(max)
		if max == 0 {
			return usageCodef("unexpected_argument", "%s takes no argument, and %q is one — "+
				"a value belongs to a flag (`--name value`), or this is a typo", fs.Name(), extra)
		}
		return usageCodef("unexpected_argument", "%s takes at most %d %s, and %q is one too many",
			fs.Name(), max, plural(max, "argument", "arguments"), extra)
	}
	return nil
}

// interspersed reorders args so every flag precedes every operand, and
// `flag` then reads them all. A flag that takes a value carries the next word
// with it; an unknown flag is left in place for `flag` to refuse in its own
// (rewritten) words.
//
// **It refuses `--enabled false`.** Go's bool flags never read the next word:
// `--enabled false` is `--enabled` (true) followed by a positional `false`,
// which is the opposite of what was written. Only `--enabled=false` says
// false. Named rather than left to the operand count, because "takes no
// argument" would not tell anybody why.
func interspersed(fs *flagSet, args []string) ([]string, error) {
	var flags, operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			operands = append(operands, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			if i+1 < len(args) {
				if word := strings.ToLower(args[i+1]); word == "true" || word == "false" {
					return nil, usageCodef("bad_flag", "%s is a switch and does not read the word after it, so %q "+
						"was not its value — write %s=%s", a, args[i+1], a, word)
				}
			}
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if len(operands) == 0 {
		return flags, nil
	}
	return append(append(flags, "--"), operands...), nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// errHelp is `--help`: not a failure, and the command stops here.
var errHelp = &helpRequested{}

type helpRequested struct{}

func (*helpRequested) Error() string { return "help requested" }

// isTerminal reports whether a file is a terminal — the switch between the live
// tree and one line per step, and one of the three signals that a run is
// unattended.
//
// It has to be a real terminal test and not "is this a character device", which
// is the obvious implementation and is wrong in the one case that matters:
// `/dev/null` is a character device, so `toft rollback < /dev/null` in a script
// would be classified as interactive and would go looking for an answer nobody
// is there to give.
//
// `go-isatty` rather than `x/term`: ui's NarrationTerminal uses it too, so the
// two terminal tests in this client are one implementation.
func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// echoAsTyped puts the flag back the way the caller wrote it.
//
// `flag` normalizes every name to one dash in its own errors, so `--nope`
// comes back as "flag provided but not defined: -nope". Every flag this CLI
// documents takes two, which makes that sentence a worked example of the wrong
// convention — and the reader most likely to copy it verbatim is an agent
// correcting its own typo, which then gets the same refusal for a second
// reason it cannot see.
//
// Only the exact single-dash spelling is rewritten, and only when the caller
// actually typed the double-dash form; anything else is left alone rather than
// guessed at.
func echoAsTyped(msg string, args []string) string {
	const marker = "flag provided but not defined: -"
	if !strings.HasPrefix(msg, marker) {
		return msg
	}
	name := msg[len(marker):]
	for _, arg := range args {
		if arg == "--"+name || strings.HasPrefix(arg, "--"+name+"=") {
			return marker + "-" + name
		}
	}
	return msg
}

// flagWords rewrites `flag`'s own sentences into this client's: `invalid
// value "noequals" for flag -param: …` and `… for flag -lines: parse error`
// named a single-dash flag nobody typed and, for a number, no expectation at
// all (D-18). Anything it does not recognise is returned as it came.
func flagWords(fs *flagSet, msg string) string {
	const needs = "flag needs an argument: -"
	if strings.HasPrefix(msg, needs) {
		name := strings.TrimLeft(msg[len(needs):], "-")
		return fmt.Sprintf("--%s needs a value after it", name)
	}
	m := invalidFlagValue.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	name := m[1]
	dash := "--"
	if len(name) == 1 {
		dash = "-" // -j, -o: written as typed
	}
	given := ""
	if v := invalidDurationValue.FindStringSubmatch(msg); v != nil {
		given = v[1]
	}
	cause := strings.TrimSpace(msg[len(m[0]):])
	// A value type of this client's already names its flag and what it wants.
	if strings.HasPrefix(cause, dash+name) {
		return cause
	}
	if f := fs.Lookup(name); f != nil {
		if g, ok := f.Value.(flag.Getter); ok {
			switch g.Get().(type) {
			case int, int64, uint, uint64:
				return fmt.Sprintf("%s%s takes a whole number, not %q", dash, name, given)
			case float64:
				return fmt.Sprintf("%s%s takes a number, not %q", dash, name, given)
			case bool:
				return fmt.Sprintf("%s%s is a switch — write %s%s, or %s%s=false — not %q", dash, name, dash, name, dash, name, given)
			}
		}
	}
	return fmt.Sprintf("%s%s does not take %q: %s", dash, name, given, cause)
}

// codedFlag is a flag value whose refusals have a code for the --json
// document. `flag` reports a value's error as a string — "invalid value … for
// flag -param: …" — so the type is gone by the time parse sees it, and the
// flag's own name in that sentence is how the code is found again.
type codedFlag interface{ refusalCode() string }

var invalidFlagValue = regexp.MustCompile(`^invalid value ".*" for flag -([A-Za-z0-9-]+): `)

func flagValueCode(fs *flagSet, err error) string {
	m := invalidFlagValue.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	if f := fs.Lookup(m[1]); f != nil {
		if c, ok := f.Value.(codedFlag); ok {
			return c.refusalCode()
		}
	}
	return ""
}

// durationRefusal is a duration flag's refusal in words that name the unit.
// `flag` says `invalid value "200" for flag -timeout: parse error`, which tells
// nobody that the number wanted a unit after it (drills 2026-10-28 #11). Every
// duration flag goes through here — `fetch --timeout`, `deploy
// --verify-timeout`, `job run --timeout`, `--wait-timeout` — so they refuse
// alike. Nil when the error is not a duration flag's.
func durationRefusal(fs *flagSet, err error) error {
	m := invalidDurationValue.FindStringSubmatch(err.Error())
	if m == nil {
		return nil
	}
	f := fs.Lookup(m[2])
	if f == nil {
		return nil
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return nil
	}
	if _, ok := g.Get().(time.Duration); !ok {
		return nil
	}
	return usageCodef("invalid_duration", "--%s takes a duration such as 30s or 2m, not %q", f.Name, m[1])
}

var invalidDurationValue = regexp.MustCompile(`^invalid value "(.*)" for flag -([A-Za-z0-9-]+): `)

// flagsSet is which flags this invocation actually named, as opposed to which
// have values. `flag` cannot tell a default from an explicit repetition of it.
func flagsSet(fs *flagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}
