import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Trash2 } from "lucide-react";
import { toast } from "sonner";
import { api, type BuildCacheState, type SystemCheck } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/notice";
import { Section } from "@/components/section";
import { formatBytes } from "@/lib/bytes";
import { RelativeTime } from "@/components/relative-time";
import { cn } from "@/lib/utils";
import { toneText } from "@/lib/tone";

/**
 * Below this, nothing any app depends on came out.
 *
 * A reclaim against an already-empty cache still moves a few hundred bytes: npm
 * recreates its index skeleton the moment `cache clean` finishes, and the two
 * walks either side of the verbs land on different bookkeeping. Reporting that
 * as a reclaim is what makes a working button look broken — an administrator
 * freeing disk during an incident reads “Reclaimed 224 B” as the platform
 * having nothing left to give, when it means the opposite: the caches a reclaim
 * can clear are *already* empty and the remaining size is not theirs.
 *
 * A mebibyte is a judgement, not a measurement — far above the tools’ own
 * bookkeeping and far below any real package.
 */
const MEANINGFUL_RECLAIM_BYTES = 1024 * 1024;

/**
 * The shared build cache, at the foot of Admin → Checks.
 *
 * **Last on the page, and its own card rather than a row in the table.** Every
 * row above it is a *check*: read-only diagnosis, which is what makes this page
 * safe to hammer during an incident. The one destructive maintenance control on
 * it has to be visibly not one of them, and it has to be somewhere nobody
 * reaches by accident on the way to the answer they opened the page for.
 *
 * **It states no fact the table already does.** The size, when it was
 * measured and the directory are the "Build cache directory" row; how full the
 * disk under it is, the "Release disk" row. Each was said twice on one page.
 * What is left is the action and what the last one did.
 */
export function BuildCacheCard({
  cache,
  cacheCheck,
}: {
  cache: BuildCacheState;
  /** The `cache_dir` row of the same report. A cache directory that is missing
   *  or unwritable is a problem this card cannot fix and must not pretend to. */
  cacheCheck?: SystemCheck;
}) {
  const queryClient = useQueryClient();
  const [starting, setStarting] = useState(false);

  const broken = cacheCheck?.status === "fail";
  const busy = cache.reclaiming || starting;
  const freed = cache.last_freed ?? 0;
  const body =
    broken || cache.reclaiming || !!cache.last_reclaim || !!cache.measure_error;

  async function reclaim() {
    setStarting(true);
    try {
      await api.reclaimBuildCache();
      // The figure and the running flag both live on the checks report, so the
      // card updates by refetching the page's own query rather than holding a
      // second copy of the same facts.
      await queryClient.invalidateQueries({ queryKey: ["system-checks"] });
      toast.success("Reclaiming the build cache");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn’t reclaim the build cache");
    } finally {
      setStarting(false);
    }
  }

  return (
    <Section
      title="Build cache"
      description="Shared by all builds. Clearing it frees disk and doesn’t affect running apps."
      actions={
        <Button size="sm" variant="outline" onClick={reclaim} disabled={busy || broken}>
          <Trash2 className="mr-1.5 h-4 w-4" />
          {busy ? "Reclaiming…" : "Reclaim"}
        </Button>
      }
      className="mt-6"
    >
      {body && (
      <div className="space-y-4 p-4">
        {/* Size, age, directory and its disk are the "Build cache directory"
            and "Release disk" rows above; this card adds only what a reclaim
            did. The last reclaim is a fact here, and a notice only while it
            is news — see below. */}
        {!cache.reclaiming && cache.last_reclaim && (
          <dl>
            <dt className="text-xs text-muted-foreground">Last reclaim</dt>
            <dd
              className={cn(
                "mt-1 text-sm font-medium tabular-nums",
                freed >= MEANINGFUL_RECLAIM_BYTES && toneText.ok,
              )}
            >
              {freed >= MEANINGFUL_RECLAIM_BYTES
                ? `${formatBytes(freed)} freed`
                : "Nothing to free"}
            </dd>
            <dd className="mt-1 text-xs text-muted-foreground">
              <RelativeTime iso={cache.last_reclaim} />
            </dd>
          </dl>
        )}

        {broken && (
          <Notice tone="danger" size="sm" title="This directory is not usable">
            Fix the “Build cache directory” check above before reclaiming.
          </Notice>
        )}

        {cache.reclaiming && (
          <Notice tone="info" size="sm" live>
            Reclaiming after the current build finishes. New builds wait until it’s done.
          </Notice>
        )}

        {!cache.reclaiming && cache.last_reclaim && (
          <Notice
            tone={cache.last_error ? "attention" : "neutral"}
            icon={null}
            size="sm"
          >
            {freed >= MEANINGFUL_RECLAIM_BYTES ? (
              <>The next build for each app will download its dependencies again.</>
            ) : (
              // The promise is conditional because it is a claim about apps: if
              // nothing came out, nothing re-downloads, and saying otherwise is
              // the copy overstating the code.
              <>
                The caches were already empty. Go’s build cache counts toward the size but is
                never cleared.
              </>
            )}
            {cache.last_error && <span className="mt-1 block">{cache.last_error}</span>}
          </Notice>
        )}

        {cache.measure_error && (
          <Notice tone="attention" size="sm" title="The last measurement didn’t finish">
            {cache.measure_error} The size in the row above is the last one that did.
          </Notice>
        )}
      </div>
      )}
    </Section>
  );
}
