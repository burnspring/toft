import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api, type UserAppUsage } from "@/lib/api";
import { formatUTCDay } from "@/lib/time";
import { Section } from "@/components/section";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { Skeleton } from "@/components/ui/skeleton";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

const WINDOWS: { days: number; label: string }[] = [
  { days: 30, label: "Last 30 days" },
  { days: 90, label: "Last 90 days" },
  { days: 365, label: "Last year" },
];

/**
 * The apps one person has been opening — what they are interested in, or have
 * been reaching — on their page under People (ADR §30's one transpose).
 *
 * The *Apps* section above it is what they **may** open; this is what they
 * **did**. Read from the same daily counters each app's Usage tab reads, so the
 * two agree, and only apps that still exist: a deleted app is not an answer
 * an administrator can act on. Their own visits only — a key's traffic is a
 * script's, not their interest.
 */
export function UserAppUsageSection({ userId }: { userId: string }) {
  const [days, setDays] = useState(90);
  const { data, isLoading, error, refetch } = useQuery<UserAppUsage>({
    queryKey: ["user-app-usage", userId, days],
    queryFn: () => api.getUserAppUsage(userId, days),
  });
  const label = (d: number) => WINDOWS.find((w) => w.days === d)?.label ?? `Last ${d} days`;
  const clipped = data?.retained_from && data.retained_from > data.since;

  return (
    <Section
      id="apps-used"
      title="Apps used"
      description="What they opened, most recent first. Apps that still exist; their own visits, not API keys."
      actions={
        <Select value={String(days)} onValueChange={(v) => setDays(Number(v))}>
          <SelectTrigger size="sm" className="w-36" aria-label="Window">
            <SelectValue>{(v) => label(Number(v))}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            {WINDOWS.map((w) => (
              <SelectItem key={w.days} value={String(w.days)}>
                {w.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      }
    >
      {isLoading ? (
        <div className="space-y-2 p-4">
          {[0, 1, 2].map((i) => (
            <Skeleton key={i} className="h-8" />
          ))}
        </div>
      ) : error ? (
        <div className="p-4">
          <QueryError
            title="What they opened could not be read"
            error={error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => void refetch()}
          />
        </div>
      ) : !data || data.apps.length === 0 ? (
        <EmptyState
          size="inline"
          title={`No apps opened in the ${label(days).toLowerCase()}`}
          description={days < 365 ? "A longer window may find some." : undefined}
        />
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">App</TableHead>
                <TableHead>Last opened</TableHead>
                <TableHead className="hidden text-right @md:table-cell">Days</TableHead>
                <TableHead className="hidden text-right @xl:table-cell">Requests</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.apps.map((a) => (
                <TableRow key={a.app_id}>
                  <TableCell className="max-w-0">
                    <Link to={`/apps/${a.app_id}`} className="block truncate font-medium text-foreground hover:underline">
                      {a.name}
                    </Link>
                    <span className="block truncate font-mono text-xs text-muted-foreground">{a.slug}</span>
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">{formatUTCDay(a.last_day)}</TableCell>
                  <TableCell className="hidden text-right text-sm tabular-nums @md:table-cell">{a.days}</TableCell>
                  <TableCell className="hidden text-right text-sm tabular-nums @xl:table-cell">
                    {a.requests.toLocaleString("en-US")}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {clipped && (
            <p className="border-t border-border/60 px-4 py-2 text-xs text-muted-foreground">
              This install keeps visits from {formatUTCDay(data.retained_from)}, so the window starts there.
            </p>
          )}
        </>
      )}
    </Section>
  );
}
