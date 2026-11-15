import { Notice } from "@/components/notice";
import { RelativeTime } from "@/components/relative-time";
import { Section } from "@/components/section";
import { type Deployment } from "@/lib/api";

/**
 * What a notebook app's owner needs to know, on the hub.
 *
 * One sentence and one fact, and the sentence is the one this panel exists for.
 * A notebook is **executed at build time with the app's own environment** —
 * that is what lets a report read DATABASE_URL, and it is the whole point of
 * the framework — and whatever the notebook prints goes into a document the
 * platform then publishes. On a public app that means the internet.
 *
 * It is copy and not a gate, deliberately. The platform cannot know which cell
 * output is a secret, and a switch that refused the environment would refuse
 * the feature. So the risk is stated where the person who wrote the notebook
 * will read it, and the build log names the variables that were exported.
 *
 * **It makes no request of its own and mutates nothing** — Overview's two rules,
 * which the schedule controls that used to sit here broke with three queries
 * and four write paths. Those are `NotebookRenderSchedule`, on the Settings
 * tab, under the other thing that makes this app rebuild itself. The
 * "Re-renders — next in…" fact went with them rather than buying a query back:
 * a figure worth a round trip is worth a field on a response the page already
 * fetches, and it is on neither.
 *
 * Absent for marimo, which is the other half of this pair rendering as a
 * difference somebody can see: a marimo app executes its notebook for each
 * viewer, so there is nothing rendered, nothing published, and nothing to
 * schedule.
 */
export function NotebookPanel({ serving }: { serving?: Deployment }) {
  return (
    <Section title="Notebook">
      <div className="space-y-4 px-4 py-4">
        {/* The sentence this panel exists for. `attention` rather than
            `danger`: nothing is broken, and nothing here is a failure — it is
            a property of the feature that the person who wrote the notebook
            has to hold in their head while they write it. */}
        <Notice
          tone="attention"
          title="Cells run with this app’s environment variables"
        >
          Whatever they print is published. Nothing is redacted.
        </Notice>

        <dl className="grid gap-x-8 gap-y-4 sm:grid-cols-2">
          <div>
            <dt className="text-xs text-muted-foreground">Last rendered</dt>
            <dd className="mt-1 text-sm">
              {/* The render happens during the deploy that is serving, so
                  "last rendered" is that deploy's finish. Said here rather than
                  stamped into the document: the platform does not rewrite an
                  app's HTML, and a banner over somebody's report is the most
                  visible possible place to start. */}
              {serving?.finished_at ? (
                <RelativeTime iso={serving.finished_at} />
              ) : (
                <span className="text-muted-foreground">Not yet</span>
              )}
            </dd>
          </div>
        </dl>
      </div>
    </Section>
  );
}
