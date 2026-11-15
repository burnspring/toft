import type { ExecutionMode } from "@/lib/api";

/** The two words, and the only two, a person ever reads for where an app runs. */
export const executionModeLabel: Record<ExecutionMode, string> = {
  shared: "Shared",
  isolated: "Isolated",
};

/**
 * What each mode *is*, in the product's words.
 *
 * One copy, because the apps list and an app's own settings had better not
 * describe the same two modes differently — that is how a reader ends up
 * believing they are three.
 */
export const executionModeDescription: Record<ExecutionMode, string> = {
  shared: "This app runs as a process inside Toft\u2019s own footprint \u2014 dense and cheap, sharing isolation properties with every other shared app.",
  isolated: "This app runs as its own workload, with its own network namespace and resource envelope. It can run background jobs.",
};

/** What the app has, as opposed to what it is set to. */
type ModeFields = { execution_mode: ExecutionMode; running_mode?: ExecutionMode };

/**
 * The mode the release that is *serving* was started under.
 *
 * An empty `running_mode` is a row written before modes were per-deployment,
 * and every one of those is a shared process — the same reading the server does
 * in `RunningModeOrShared`. Anything showing a person where an app runs reads
 * this and never `execution_mode`, which is a setting for the next deploy.
 */
export function runningMode(app: ModeFields): ExecutionMode {
  return app.running_mode || "shared";
}

/**
 * The mode this app moves to on its next deploy, or undefined when it is
 * already there.
 *
 * The gap between the two is not a fault and not a pending job: it is what
 * "takes effect on the next deploy" looks like from the outside, and it is the
 * reason changing the setting disturbs nothing that is running.
 */
export function pendingMode(app: ModeFields): ExecutionMode | undefined {
  return app.execution_mode !== runningMode(app) ? app.execution_mode : undefined;
}

/**
 * Whether anything in this list is isolated, running or set to be.
 *
 * This is the gate on the apps list's Mode **column**, and it is deliberately
 * *not* the gate on the Mode filter beside it. The two ask different questions
 * and the original rule — show both above two distinct modes — answered only
 * the filter's.
 *
 * A filter offering one option narrows nothing, so it renders nothing whatever
 * that option is. A column is a statement about each row, and the two modes are
 * not peers: shared is what an app is by default and shows on installs that
 * have never heard of isolation, so a column reading "Shared" on every row is
 * noise. Isolated is the notable state, and an install where *every* app is
 * isolated is the one an operator most wants that confirmed on — which is
 * precisely the case a symmetric rule hid, because one distinct mode was
 * present and it counted them rather than reading them.
 *
 * The install's capability is the wrong gate here, tempting as it is: an
 * install that offers isolation and has no isolated app yet would grow a column
 * saying "Shared" about everything, which is the noise this avoids.
 */
export function anyIsolated(apps: ModeFields[]): boolean {
  return apps.some((a) => runningMode(a) === "isolated" || a.execution_mode === "isolated");
}
