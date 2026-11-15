import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { CopyableText } from "@/components/copyable-text";
import { FieldHelp } from "@/components/field-help";
import { RelativeTime } from "@/components/relative-time";
import { StatusBadge } from "@/components/status-badge";
import { describeCron } from "@/lib/cron-describe";
import { splitSchedule } from "@/lib/schedule";

/** The fields both one-per-app schedules share. */
export interface DeclaredScheduleRow {
  schedule: string;
  enabled: boolean;
  next_run_at?: string;
}

/**
 * One of an app's toft.toml schedules — `[restart]` or `[render]` — as the
 * Settings tab shows it: read-only, because the file is the only writer
 * (ADR §144), with one control, Pause/Resume, which lasts until
 * the next deploy. Reversible and audited, so it acts and toasts.
 *
 * With nothing declared it shows the block to paste, as the jobs tab's New job
 * helper does, and saves nothing.
 */
export function DeclaredSchedule({
  table,
  row,
  example,
  pending,
  onPause,
  children,
}: {
  table: "restart" | "render";
  row: DeclaredScheduleRow | null | undefined;
  /** The block to paste when the table is absent. */
  example: string;
  pending: boolean;
  onPause: (enabled: boolean) => void;
  /** What else this schedule says — the restart's last outcome. */
  children?: ReactNode;
}) {
  if (!row) {
    return (
      <div className="space-y-2">
        <FieldHelp>
          Declared in the app’s toft.toml. Add this and deploy:
        </FieldHelp>
        <CopyableText text={example} subject={`[${table}] block`} />
      </div>
    );
  }
  const { zone, expr } = splitSchedule(row.schedule);
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <div className="min-w-0 flex-1">
          <div className="text-sm text-foreground">
            {describeCron(expr) ?? <span className="font-mono">{expr}</span>}
            <span className="text-muted-foreground"> · {zone || "UTC"}</span>
          </div>
          <div className="text-xs text-muted-foreground">
            {row.enabled && row.next_run_at ? (
              <>Next <RelativeTime iso={row.next_run_at} /> · </>
            ) : null}
            Declared in toft.toml as <span className="font-mono">[{table}]</span>
          </div>
        </div>
        {!row.enabled && <StatusBadge status="paused" />}
        <Button size="sm" variant="outline" disabled={pending} onClick={() => onPause(!row.enabled)}>
          {pending ? (row.enabled ? "Pausing…" : "Resuming…") : row.enabled ? "Pause" : "Resume"}
        </Button>
      </div>
      {children}
      <FieldHelp>
        A pause lasts until the next deploy. To change the schedule, edit <span className="font-mono">[{table}]</span>{" "}
        in toft.toml.
      </FieldHelp>
    </div>
  );
}
