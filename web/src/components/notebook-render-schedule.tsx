import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { toneText } from "@/lib/tone";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Notice } from "@/components/notice";
import { RelativeTime } from "@/components/relative-time";
import { TableCard } from "@/components/table-card";
import { X } from "lucide-react";
import { SectionHeading } from "@/components/section";
import { StatusBadge } from "@/components/status-badge";
import { DeclaredSchedule } from "@/components/declared-schedule";
import {
  api,
  type RenderSchedule,
  type ScheduleRecipient,
} from "@/lib/api";

const RENDER_EXAMPLE = `[render]
schedule = "0 6 * * 1"
timezone = "Europe/London"   # optional; absent is UTC`;

/**
 * A document app's re-render schedule — notebook, R Markdown or Quarto — and
 * who the report reaches.
 *
 * The schedule is the app's toft.toml `[render]` (ADR §144): it
 * versions with the report and is reviewed in the same diff, so this shows it
 * and pauses it and nothing else. The recipient list stays here, because it is
 * people — accounts in a repository would be user ids in a commit (D14).
 *
 * On the Settings tab, under auto-deploy: "what makes this app rebuild itself"
 * is one question, so it is one place.
 */
export function NotebookRenderSchedule({ appId }: { appId: string }) {
  const queryClient = useQueryClient();

  const { data: schedule, isLoading } = useQuery({
    queryKey: ["render-schedule", appId],
    queryFn: () => api.getRenderSchedule(appId),
  });
  // The list rides on the schedule, so this never renders one belonging to a
  // schedule it has not loaded.
  const recipients = schedule?.recipients ?? [];

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ["render-schedule", appId] });
    queryClient.invalidateQueries({ queryKey: ["system-scheduled"] });
  };

  const pause = useMutation({
    mutationFn: (enabled: boolean) => api.pauseRenderSchedule(appId, enabled),
    onSuccess: (s) => {
      invalidate();
      toast.success(s.enabled ? "Re-rendering resumed" : "Re-rendering paused until the next deploy");
    },
    onError: (e: Error) => toast.error(e.message || "Couldn’t change the schedule"),
  });

  const saveRecipients = useMutation({
    mutationFn: (ids: string[]) => api.setRenderScheduleRecipients(appId, ids),
    onSuccess: (s) => {
      // The server's fresh list goes into the cache at once: the buttons
      // re-enable before an un-awaited refetch lands, and a second click in
      // that window computed the whole list from the stale one — re-adding
      // the person just removed (code review 2026-11-11, O-4).
      queryClient.setQueryData(["render-schedule", appId], s);
      invalidate();
      toast.success("Saved");
    },
    onError: (e: Error) => toast.error(e.message || "Couldn’t save the recipients"),
  });

  if (isLoading) return null;

  return (
    <div id="render-schedule" className="space-y-4">
      <DeclaredSchedule
        table="render"
        row={schedule}
        example={RENDER_EXAMPLE}
        pending={pause.isPending}
        onPause={(on) => pause.mutate(on)}
      />
      {schedule ? (
        <ReportRecipients
          appId={appId}
          schedule={schedule}
          recipients={recipients}
          saving={saveRecipients.isPending}
          onSave={(ids) => saveRecipients.mutate(ids)}
        />
      ) : (
        <p className="text-xs text-muted-foreground">
          Recipients can be chosen once the app’s toft.toml declares <span className="font-mono">[render]</span>.
        </p>
      )}
    </div>
  );
}

// ── Who the report reaches ──────────────────────────────────

/**
 * The people told when this report re-renders.
 *
 * **A link, not the report**, and the copy says so: an attachment would be a
 * copy of the data outside every access control this product has, and what goes
 * out is a note with a link behind the app's own sign-in.
 *
 * Which is also why the picker is narrowed to people who can already open the
 * app. A link is useless to somebody who is refused at it, so the access list
 * and the recipient list are the same set — and the platform re-checks that on
 * every send rather than trusting what was true when somebody was added.
 */
function ReportRecipients({
  appId, schedule, recipients, saving, onSave,
}: {
  appId: string;
  schedule: RenderSchedule;
  recipients: ScheduleRecipient[];
  saving: boolean;
  onSave: (ids: string[]) => void;
}) {
  const [search, setSearch] = useState("");
  const q = search.trim();

  const {
    data: candidates = [],
    isFetching: searching,
    error: searchError,
  } = useQuery({
    queryKey: ["directory-users", q],
    queryFn: () => api.searchDirectoryUsers(q),
    enabled: q.length >= 2,
  });
  const { data: history } = useQuery({
    queryKey: ["report-deliveries", appId],
    queryFn: () => api.listReportDeliveries(appId),
    enabled: recipients.length > 0,
  });

  const ids = new Set(recipients.map((r) => r.user_id));
  const addable = candidates.filter((u) => !ids.has(u.id));
  const last = history?.deliveries?.[0];

  return (
    <div className="space-y-2 border-t border-border/60 pt-3">
      <SectionHeading
        level={3}
        title="Send it to"
        description="Each re-render emails them a link. They need access to the app to open it."
      />

      {recipients.length === 0 ? (
        <p className="text-xs text-muted-foreground">Nobody yet.</p>
      ) : (
        <TableCard className="divide-y divide-border/60" flat>
          {recipients.map((r) => (
            <div key={r.user_id} className="flex items-center gap-2.5 px-3 py-2">
              <span className="text-sm font-medium text-foreground">{r.login}</span>
              {r.email ? (
                <span className="min-w-0 truncate text-xs text-muted-foreground">{r.email}</span>
              ) : (
                // Named back rather than discovered on the first send: the
                // picker carries no address, so this is visible nowhere else.
                <StatusBadge status="no-address" tone="attention" label="No address" />
              )}
              <Button
                variant="ghost"
                size="icon-xs"
                className="ml-auto"
                aria-label={`Remove ${r.login}`}
                disabled={saving}
                onClick={() => onSave(recipients.filter((x) => x.user_id !== r.user_id).map((x) => x.user_id))}
              >
                <X className="h-3.5 w-3.5" />
              </Button>
            </div>
          ))}
        </TableCard>
      )}

      <div className="space-y-1.5">
        <Label htmlFor="report-recipient-search" className="sr-only">Add people</Label>
        <Input
          id="report-recipient-search"
          placeholder="Add people by name…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          autoComplete="off"
        />
        {q.length >= 2 && (
          <div className="max-h-40 divide-y divide-border/60 overflow-y-auto rounded-md border border-border/60">
            {/* Searching and failed are not "no matches" — the three-state
                rule the Access tab's grant search keeps. */}
            {searchError ? (
              <p className={`p-2.5 text-xs ${toneText.danger}`}>
                {searchError instanceof Error && searchError.message
                  ? searchError.message
                  : "The directory could not be searched."}
              </p>
            ) : searching && addable.length === 0 ? (
              <p className="p-2.5 text-xs text-muted-foreground">Searching…</p>
            ) : addable.length === 0 ? (
              <p className="p-2.5 text-xs text-muted-foreground">No matching people.</p>
            ) : (
              addable.map((u) => (
                <button
                  key={u.id}
                  type="button"
                  disabled={saving}
                  className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:opacity-50"
                  onClick={() => {
                    onSave([...recipients.map((x) => x.user_id), u.id]);
                    setSearch("");
                  }}
                >
                  <span className="font-medium">{u.login}</span>
                </button>
              ))
            )}
          </div>
        )}
      </div>

      {last && (
        // The record, where its owner can read it. Without this the whole
        // history of the feature lives in a table only an administrator sees.
        <p className="text-xs text-muted-foreground">
          Last sent <RelativeTime iso={last.created_at} /> — {last.delivered} delivered
          {last.skipped > 0 && `, ${last.skipped} skipped`}
          {last.failed > 0 && `, ${last.failed} failed`}.
          {(last.skipped > 0 || last.failed > 0) && (
            <> {last.recipients?.find((r) => r.reason)?.reason}</>
          )}
        </p>
      )}

      {!schedule.enabled && (
        <Notice tone="neutral" size="sm" title="Re-rendering is paused">
          Nothing is sent while the schedule is paused. The list is kept.
        </Notice>
      )}
    </div>
  );
}
