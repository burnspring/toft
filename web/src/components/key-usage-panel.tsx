import { useState } from "react";
import { toneText } from "@/lib/tone";
import { useQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { QueryError } from "@/components/query-error";
import { EmptyState } from "@/components/empty-state";
import { KeyUsageChart } from "@/components/key-usage-chart";
import type { KeyUsage, KeyUsageBucket } from "@/lib/api";

/** Windows offered per bucket. A year of days would be 365 bars in 640px; a
 * week of months would be one. Each bucket gets the ranges it can actually
 * draw. */
const RANGES: Record<KeyUsageBucket, { days: number; label: string }[]> = {
  day: [
    { days: 7, label: "7 days" },
    { days: 30, label: "30 days" },
    { days: 90, label: "90 days" },
  ],
  week: [
    { days: 90, label: "13 weeks" },
    { days: 365, label: "1 year" },
  ],
  month: [{ days: 365, label: "1 year" }],
};

const BUCKETS: KeyUsageBucket[] = ["day", "week", "month"];

/**
 * Traffic for one API key, shared by the personal-keys page and the app-detail
 * key list — the question is the same for both credential classes and so is
 * the table underneath.
 */
export function KeyUsagePanel({
  fetchUsage,
  queryKey,
}: {
  fetchUsage: (bucket: KeyUsageBucket, days: number) => Promise<KeyUsage>;
  queryKey: (string | number)[];
}) {
  const [bucket, setBucket] = useState<KeyUsageBucket>("day");
  const [days, setDays] = useState(30);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: [...queryKey, bucket, days],
    queryFn: () => fetchUsage(bucket, days),
  });

  function switchBucket(next: KeyUsageBucket) {
    setBucket(next);
    // The current window may not be offered for the new bucket; fall back to
    // its widest rather than silently requesting something it cannot draw.
    const ranges = RANGES[next];
    if (!ranges.some((r) => r.days === days)) {
      setDays(ranges[ranges.length - 1].days);
    }
  }

  const totals = data?.totals;
  const hasTraffic = (totals?.requests ?? 0) > 0;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        {/* `aria-pressed`, not the fill. Two toggle rows in the same corner of
            the same panel, each carrying its whole state in a button's
            background — the rule `ChoiceButtons` and `FilterBar` both follow,
            and these two were copied for the rendering rather than the
            semantics. Named groups, because "Daily" and "7 days" beside each
            other say nothing about which axis they are on. */}
        <div role="group" aria-label="Group traffic by" className="flex gap-1">
          {BUCKETS.map((b) => (
            <Button
              key={b}
              size="sm"
              variant={bucket === b ? "secondary" : "ghost"}
              aria-pressed={bucket === b}
              onClick={() => switchBucket(b)}
            >
              {b === "day" ? "Daily" : b === "week" ? "Weekly" : "Monthly"}
            </Button>
          ))}
        </div>
        <div role="group" aria-label="How far back" className="flex gap-1">
          {RANGES[bucket].map((r) => (
            <Button
              key={r.days}
              size="sm"
              variant={days === r.days ? "secondary" : "ghost"}
              aria-pressed={days === r.days}
              onClick={() => setDays(r.days)}
            >
              {r.label}
            </Button>
          ))}
        </div>
      </div>

      {isLoading ? (
        <Skeleton className="h-40 w-full" />
      ) : error ? (
        // Before the no-traffic line, which would otherwise report a failed
        // read as a key nobody has used — the opposite of the fact somebody
        // opened this panel to check.
        <QueryError
          title="This key’s usage could not be read"
          error={error}
          fallback="Only the key’s owner and administrators can see its usage."
          onRetry={() => void refetch()}
        />
      ) : !hasTraffic ? (
        <EmptyState
          size="inline"
          title="No calls recorded in this window"
          description="Counts appear within a minute of the first request."
        />
      ) : (
        <>
          <div className="flex flex-wrap gap-x-6 gap-y-1 text-sm">
            <span>
              <span className="font-medium">{totals!.requests.toLocaleString()}</span>{" "}
              <span className="text-muted-foreground">calls</span>
            </span>
            {totals!.client_error > 0 && (
              <span className={toneText.attention}>
                {totals!.client_error.toLocaleString()} rejected
              </span>
            )}
            {totals!.server_error > 0 && (
              <span className={toneText.danger}>
                {totals!.server_error.toLocaleString()} failed
              </span>
            )}
          </div>

          <KeyUsageChart series={data!.series} bucket={bucket} />

          {data!.targets.length > 0 && (
            <div className="pt-1">
              <p className="mb-1.5 text-xs font-medium text-muted-foreground">Called</p>
              <ul className="space-y-1">
                {data!.targets.map((t) => (
                  <li key={t.app_id || "platform"} className="flex justify-between text-xs">
                    <span className="text-foreground">
                      {t.app_id === ""
                        ? "The Toft API"
                        : t.app_name || "an app that no longer exists"}
                    </span>
                    <span className="text-muted-foreground">{t.requests.toLocaleString()}</span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </>
      )}
    </div>
  );
}
