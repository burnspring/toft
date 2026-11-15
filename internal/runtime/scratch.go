package runtime

import (
	"path/filepath"
)

// EnvScratchDir is the explicit, documented path of the app's scratch
// directory. TMPDIR points at the same place — that is what most frameworks
// and standard libraries pick up automatically — while this name is the one
// the SDKs read, so an app can rely on it without knowing whether something
// else has repointed TMPDIR. The isolated runner exports the same pair
// (runner.EnvScratchDir); the runner package is deliberately standalone, so
// the name is stated in both.
const EnvScratchDir = "TOFT_SCRATCH_DIR"

// ScratchDir is the per-app ephemeral directory, apps/<id>/scratch — beside
// the builder's release tree, but owned by the runtime because its lifecycle
// is bound to workload starts: wiped on every start — and since Deploy is the
// only start, there is no second path that could forget to — never backed up,
// never shared between apps. Nothing an app writes here survives its process.
func ScratchDir(appsDir, appID string) string {
	return filepath.Join(appsDir, appID, "scratch")
}

// The app's own identity in its process environment. Named here beside
// EnvScratchDir because they are injected together and for the same reason: an
// app should not have to be told where it is or who it is by an administrator
// editing its Settings tab.
//
// EnvAppID is set even though no route under /api/v1/app accepts an app id —
// that surface is entirely self-referential, which is what makes an IDOR
// impossible in it. It exists so an app can put its own id in its own logs.
const (
	EnvAppToken = "TOFT_APP_TOKEN"
	EnvAppID    = "TOFT_APP_ID"
	EnvAppSlug  = "TOFT_APP_SLUG"
	EnvAPIURL   = "TOFT_API_URL"
	// EnvRoleKey is the key this workload verifies X-Toft-Role with: per
	// app, per start, derived by the platform and never stored anywhere it
	// could be read back. It sits beside the app token because it travels the
	// same way and lives exactly as long — a redeploy is a new workload name
	// and therefore a new key, which invalidates every assertion minted for the
	// previous start.
	//
	// The isolated runner exports the same variable under the same name
	// (runner.EnvRoleKey), so an app verifies identically in both execution
	// modes; that package is deliberately standalone, so the name is stated in
	// both.
	EnvRoleKey = "TOFT_ROLE_KEY"
)
