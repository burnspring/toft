// The command-line client. Its own module, so that lipgloss — and every
// dependency the client acquires later — stays out of the server's module
// graph entirely. The server binary is the thing an enterprise vendors and
// audits, and a terminal styling library has no business in its go.sum.
//
// It imports the parent's internal/deploysteps through the replace below,
// which a nested module may do: the internal rule is satisfied by the shared
// `toft/` import-path prefix. That import is the whole reason this lives
// in the same repository — the CLI's progress display and the API's frames are
// the same nouns, and a client elsewhere would restate the step keys and drift
// from them.
//
// **No go.work is committed.** A workspace file changes how the server module
// resolves its own dependencies for everyone who checks the repository out.
module toft/cli

go 1.27.0

toolchain go1.27.3

require (
	charm.land/bubbletea/v2 v2.1.1
	charm.land/lipgloss/v2 v2.0.6
	github.com/BurntSushi/toml v1.6.0
	github.com/charmbracelet/colorprofile v0.4.3
	github.com/charmbracelet/x/ansi v0.11.9
	github.com/charmbracelet/x/term v0.2.2
	github.com/joho/godotenv v1.5.1
	github.com/mattn/go-isatty v0.0.23
	toft v0.0.0-00010101000000-000000000000
)

require (
	github.com/charmbracelet/ultraviolet v0.0.0-20261008173134-6b8d4baf91b4 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mattn/go-runewidth v0.0.30 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/xo/terminfo v1.0.0 // indirect
	golang.org/x/exp v0.0.0-20260718201538-764159d718ef // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace toft => ../
