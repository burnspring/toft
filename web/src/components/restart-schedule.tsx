import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { DeclaredSchedule } from "@/components/declared-schedule";
import { RelativeTime } from "@/components/relative-time";
import { StatusBadge } from "@/components/status-badge";
import { FieldHelp } from "@/components/field-help";
import { api, type RestartSchedule as Row } from "@/lib/api";

const RESTART_EXAMPLE = `[restart]
schedule = "0 4 * * *"
timezone = "Europe/London"   # optional; absent is UTC`;

/** Why a firing was skipped, as a sentence. The server's words are the list. */
const SKIPPED: Record<string, string> = {
  sleeping: "the app was asleep",
  stopped: "the app was stopped",
  crashed: "the app had crashed, and stays as the platform left it",
  deploy_pending: "a deploy was under way",
  locked: "the app is locked",
  no_process: "the app had no process to restart",
};

/**
 * The app's scheduled restart — `[restart]` in toft.toml (ADR §144, §145).
 * Read-only with one control, Pause/Resume, and the last firing's
 * outcome: a firing restarts a running app and nothing else, so "did last
 * night's restart happen" always has an answer here.
 */
export function RestartScheduleBlock({ appId, shared }: { appId: string; shared: boolean }) {
  const queryClient = useQueryClient();
  const { data: row, isLoading } = useQuery({
    queryKey: ["restart-schedule", appId],
    queryFn: () => api.getRestartSchedule(appId),
  });

  const pause = useMutation({
    mutationFn: (enabled: boolean) => api.pauseRestartSchedule(appId, enabled),
    onSuccess: (s) => {
      queryClient.invalidateQueries({ queryKey: ["restart-schedule", appId] });
      queryClient.invalidateQueries({ queryKey: ["system-scheduled"] });
      toast.success(s.enabled ? "Scheduled restarts resumed" : "Scheduled restarts paused until the next deploy");
    },
    onError: (e: Error) => toast.error(e.message || "Couldn’t change the schedule"),
  });

  if (isLoading) return null;

  return (
    <div id="restart-schedule" className="space-y-3">
      <div className="space-y-1">
        <p className="text-sm font-medium text-foreground">Restart on a schedule</p>
        <div className="text-xs text-muted-foreground">
          Restarts the app while it is running. Asleep, stopped or crashed, it is left alone.
        </div>
      </div>
      <DeclaredSchedule
        table="restart"
        row={row}
        example={RESTART_EXAMPLE}
        pending={pause.isPending}
        onPause={(on) => pause.mutate(on)}
      >
        {row && <LastOutcome row={row} />}
      </DeclaredSchedule>
      {row && shared && (
        <FieldHelp>Visitors see “not responding” for the app’s start time at each restart.</FieldHelp>
      )}
    </div>
  );
}

function LastOutcome({ row }: { row: Row }) {
  if (!row.last_fired_at || !row.last_outcome) {
    return <p className="text-xs text-muted-foreground">Not fired yet.</p>;
  }
  const when = <RelativeTime iso={row.last_fired_at} />;
  switch (row.last_outcome) {
    case "restarted":
      return (
        <p className="flex items-center gap-2 text-xs text-muted-foreground">
          <StatusBadge status="restarted" tone="ok" label="Restarted" /> {when}
        </p>
      );
    case "skipped":
      return (
        <p className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <StatusBadge status="skipped" /> {when} — {SKIPPED[row.last_reason ?? ""] ?? row.last_reason}
        </p>
      );
    default:
      return (
        <p className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <StatusBadge status="failed" label="Failed" /> {when} — {row.last_reason}
        </p>
      );
  }
}
