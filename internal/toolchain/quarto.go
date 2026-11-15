package toolchain

import (
	"os"
	"strings"
	"sync"
)

// Quarto, discovered where the build runs.
//
// **It is not a language, and it is deliberately not in `Languages`.** Nothing
// here resolves a version against a pin, nothing picks between several
// installations, and no framework maps to a "quarto family" image. It is one
// program that either exists on this host or does not, and the whole of the
// question is which.
//
// **Why it is discovered rather than installed.** `quarto-cli` on PyPI is an
// sdist that downloads its own toolchain during install: six minutes, and
// 892 MB unpacked — deno, pandoc, typst, esbuild, dart-sass. Three places it
// could live and only one survives:
//
//   - In the app's venv. It lands in the release directory, so Publish packs it:
//     892 MB per release in the artifact store, re-fetched on every pod start,
//     multiplied by version retention.
//   - In the runner images. Never needed there. The build runs on the platform
//     host in *both* execution modes, and a rendered notebook's workload is a
//     file server — so this would grow the Python image every Flask, FastAPI
//     and Streamlit app also pulls, for a tool none of them runs.
//   - Where the build runs, named by the operator. This one.
//
// So it follows the rule the language toolchains already follow: the operator
// says where, and the probe says what. Absent means the engine is refused by
// name and nothing else changes — no UI offers an engine the install cannot
// run, and nbconvert is untouched.

// QuartoBinEnv names where an operator put Quarto. Colon-separated like the
// TOFT_*_PATH variables, because an operator who has two of them should not
// have to learn a second convention for this one.
const QuartoBinEnv = "QUARTO_BIN"

// quartoBinary is what Quarto is called when nobody has said where it is.
const quartoBinary = "quarto"

type quartoResult struct {
	path    string
	version string
}

var (
	quartoOnce sync.Once
	quartoInfo quartoResult
)

// Quarto reports the Quarto this host has: the path to run and the version it
// answered with. Both empty when there is none.
//
// Probed once, like every other toolchain fact, because an operator does not
// install Quarto halfway through the afternoon and a `--version` exec per deploy
// buys nothing.
func Quarto() (path, version string) {
	quartoOnce.Do(func() { quartoInfo = probeQuarto() })
	return quartoInfo.path, quartoInfo.version
}

// QuartoNow is Quarto with one correction, for a caller that reports rather
// than resolves: when the cached answer is empty, the probe is run again now.
//
// The same rule and the same reason as toolchain.AvailableNow. The comment
// above is right that nobody installs Quarto halfway through the afternoon —
// except the operator who has just read "Not installed" on the system checks
// page, who is the one caller this exists for. Never stored, so HasQuarto and
// every refusal that reads it stay fixed for the life of the process.
func QuartoNow() (path, version string) {
	if p, v := Quarto(); p != "" {
		return p, v
	}
	r := probeQuarto()
	return r.path, r.version
}

// HasQuarto is the question every caller actually asks.
func HasQuarto() bool {
	path, _ := Quarto()
	return path != ""
}

func probeQuarto() quartoResult {
	for _, candidate := range quartoCandidates() {
		out, err := runVersion(candidate, []string{"--version"})
		if err != nil {
			continue
		}
		// `quarto --version` prints the bare version and nothing else. Kept
		// unparsed past the first line: Quarto is not resolved against a
		// constraint, so this is a fact for a log line and an admin page rather
		// than an input to a decision.
		v := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
		if v == "" {
			continue
		}
		return quartoResult{path: candidate, version: v}
	}
	return quartoResult{}
}

// quartoCandidates is QUARTO_BIN's entries, then the bare name for PATH.
func quartoCandidates() []string {
	var out []string
	if raw := strings.TrimSpace(os.Getenv(QuartoBinEnv)); raw != "" {
		for _, p := range strings.Split(raw, ":") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return append(out, quartoBinary)
}
