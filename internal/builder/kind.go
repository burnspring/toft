package builder

// The three kinds of app, by what answers a request (ADR §107):
//
//   - serving: a process answers every request — Flask, Next.js, Shiny, marimo…
//   - rendered: the build produced files and they are served as they are — a
//     static folder, or a document rendered at build time (Rerenderable)
//   - batch: nothing answers; the app is its jobs
//
// Jobs are not a kind. Any app may declare them, so "has jobs" is counted
// beside these three and overlaps them.
const (
	AppKindServing  = "serving"
	AppKindRendered = "rendered"
	AppKindBatch    = "batch"
)

// AppKind is the kind a framework string makes an app. The SPA's `appKind`
// (web/src/lib/frameworks.ts) is the same table.
func AppKind(framework string) string {
	switch {
	case IsBatchFramework(framework):
		return AppKindBatch
	case framework == string(FrameworkStatic) || Rerenderable(framework):
		return AppKindRendered
	}
	return AppKindServing
}
