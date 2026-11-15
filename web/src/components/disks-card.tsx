import type { DisksBlock } from "@/lib/api";
import { Notice } from "@/components/notice";
import { Progress } from "@/components/ui/progress";
import { Section } from "@/components/section";
import { formatBytes } from "@/lib/bytes";
import { diskTone, fullnessLabel } from "@/lib/disks";
import { toneSolid, toneText } from "@/lib/tone";
import { cn } from "@/lib/utils";

/**
 * Disks this install writes to, as how full each is. Admin → Checks passes
 * only the ones its table has no row for (`disksBeyondTheRows`).
 *
 * A size on its own cannot be acted on — 40 GB is nothing on a 2 TB volume and
 * an emergency on a 50 GB one — so each row leads with the fraction and draws
 * it, and the folders on that disk are listed under it in mono, because the
 * path is what an operator takes to a shell. Fullest first, from the server.
 *
 * Measured when the report was asked for (statfs, one call per folder) and at
 * no other time; this card adds no polling of its own.
 */
export function DisksCard({ block }: { block: DisksBlock }) {
  if (block.disks.length === 0 && !block.unanswered?.length) return null;
  return (
    <Section
      id="disks"
      title="Disks"
      description="Disks the checks above don’t measure, as of this report."
      className="mt-6"
    >
      <ul className="divide-y divide-border/60">
        {block.disks.map((d) => {
          const tone = diskTone(d);
          const where = d.folders.map((f) => f.what).join(", ");
          return (
            <li key={d.folders[0].path} className="space-y-2 px-4 py-3">
              <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
                <span className={cn("text-sm font-semibold tabular-nums", tone && toneText[tone])}>
                  {d.unbounded ? "Network filesystem — no fixed size" : fullnessLabel(d)}
                </span>
                <span className="text-xs text-muted-foreground tabular-nums">
                  {d.unbounded
                    ? `${formatBytes(d.used_bytes)} used`
                    : `${formatBytes(d.used_bytes)} used of ${formatBytes(d.total_bytes)} · ${formatBytes(d.free_bytes)} free`}
                </span>
              </div>
              {!d.unbounded && (
                <Progress
                  value={Math.min(100, d.used_percent)}
                  aria-label={`${where}: ${fullnessLabel(d)}`}
                  className="h-2"
                  indicatorClassName={tone ? toneSolid[tone] : undefined}
                />
              )}
              <ul className="space-y-0.5">
                {d.folders.map((f) => (
                  <li key={f.path} className="flex flex-wrap items-baseline gap-x-2 text-xs">
                    <span className="text-muted-foreground">{f.what}</span>
                    <code className="min-w-0 font-mono break-all text-foreground">{f.path}</code>
                  </li>
                ))}
              </ul>
            </li>
          );
        })}
      </ul>
      {block.unanswered?.length ? (
        <div className="px-4 pb-4">
          <Notice tone="attention" size="sm" title="Some disks didn’t answer">
            {block.unanswered.map((u) => (
              <span key={u.path} className="block">
                {u.what} (<code className="font-mono">{u.path}</code>): {u.error}
              </span>
            ))}
          </Notice>
        </div>
      ) : null}
    </Section>
  );
}
