import { Fragment, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, Send } from "lucide-react";

import { api, type IntegrationSend } from "@/lib/api";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/page-header";
import { SearchInput } from "@/components/search-input";
import { FilterBar, FilterChips } from "@/components/filter-bar";
import { formatBytes } from "@/lib/bytes";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useUrlParam } from "@/hooks/use-list-state";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { TableCard } from "@/components/table-card";
import { RelativeTime } from "@/components/relative-time";
import { SendStatusBadge } from "@/components/send-status";

/**
 * Every message an app has asked Toft to send.
 *
 * This page is a **control**, not a report. Apps may email any address, inside
 * the company or outside it, because the case the feature exists for is a
 * report to people who have no Toft account — which means there is no
 * allow-list standing between an app and the internet, and this record is what
 * stands there instead.
 *
 * Two consequences shape the page. The recipient filter leads, because "which
 * app emailed this person" is the question an incident actually opens with.
 * And the page keeps working when the integrations are switched off, for the
 * same reason /admin/api-keys does: that is exactly when a leaked one has to be
 * found.
 */

/** The kind filter's options are built from what the server says this build
 * has, so a kind that ships is filterable the day it ships and a kind that does
 * not exist is never offered. A hand-written list here drifted from the catalog
 * the first time anything changed. */
function kindOptions(kinds: string[] | undefined, titles: Record<string, string> | undefined) {
  return [
    { label: "All", value: "" },
    ...(kinds ?? []).map((k) => ({ label: kindTitle(k, titles), value: k })),
  ];
}

/** The catalog's own name for a kind. The slug is the fallback, as written —
 * capitalizing it made "Gdrive" and "Pagerduty". */
function kindTitle(kind: string, titles: Record<string, string> | undefined): string {
  return titles?.[kind] ?? kind;
}

const statuses = [
  { label: "All", value: "" },
  { label: "Sent", value: "sent" },
  { label: "Failed", value: "failed" },
  { label: "Blocked", value: "blocked" },
  { label: "Abandoned", value: "abandoned" },
  { label: "Queued", value: "queued" },
];

/** A Select's stand-in for "no narrowing", which the URL spells as absent. */
const ALL = "all";

const windows = [
  { label: "7 days", value: "7" },
  { label: "30 days", value: "30" },
  { label: "90 days", value: "90" },
  { label: "All time", value: ALL },
];

/** How many rows one read of this feed returns. There is no cursor here, so
 *  the page says when it has stopped at the cap rather than presenting its
 *  first page as the whole log. */
const FEED_LIMIT = 200;

const PAGE_DESCRIPTION = "Every message apps have sent, including ones that didn’t arrive.";

export function AdminIntegrationLogPage() {
  // In the URL, so "every failed Slack send this week" is a link.
  const [kind, setKind] = useUrlParam("kind");
  const [status, setStatus] = useUrlParam("outcome");
  // "token" narrows to access-token issuances, which share the ledger with
  // sends but put nothing anywhere.
  const [action, setAction] = useUrlParam("action");
  const [recipient, setRecipient] = useUrlParam("to");
  const [days, setDays] = useUrlParam("window", "30");

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["integration-sends", kind, action, status, recipient, days],
    queryFn: () =>
      api.listIntegrationSends({
        kind: kind || undefined,
        action: action || undefined,
        status: status || undefined,
        // Matched whole, not by substring: the index answers containment, and
        // a partial match would quietly miss the address somebody pasted with
        // different capitalisation.
        recipient: recipient.trim() || undefined,
        days: days && days !== ALL ? Number(days) : undefined,
        limit: FEED_LIMIT,
      }),
    retry: false,
  });

  const sends = data?.sends ?? [];
  // A feed that stops at its cap and says nothing reads as the whole record.
  // This one is an audit of every address the install's apps have emailed, so
  // "that is all of them" is exactly the wrong thing for it to imply.
  const capped = sends.length >= FEED_LIMIT;

  if (error) {
    return (
      <div className="space-y-6">
        <PageHeader title="Integration log" description={PAGE_DESCRIPTION} />
        <QueryError
          title="The integration log could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <PageHeader title="Integration log" description={PAGE_DESCRIPTION} />

      <div className="space-y-3">
        {/* One row of narrowing and one strip, where there were three
            unlabelled chip groups (seven rows on a phone) over a one-row
            table. Outcome is the primary axis — "Failed" is what somebody
            opens this page for — and kind and window are the facets beside
            the search. Uncounted: this log is server-narrowed, so a count
            would count only the rows that happen to be loaded. */}
        <div className="flex flex-wrap items-center gap-2">
          <SearchInput
            value={recipient}
            onChange={setRecipient}
            placeholder="Search by address an app emailed…"
            className="w-full sm:max-w-sm"
          />
          <Select value={kind || ALL} onValueChange={(v) => setKind(v === ALL || !v ? "" : String(v))}>
            <SelectTrigger size="sm" aria-label="Channel">
              <SelectValue>
                {(v) => (v && v !== ALL ? `Channel: ${kindTitle(String(v), data?.kind_titles)}` : "All channels")}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              {kindOptions(data?.kinds, data?.kind_titles).map((o) => (
                <SelectItem key={o.value || ALL} value={o.value || ALL}>
                  {o.value ? o.label : "All channels"}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select
            value={action || ALL}
            onValueChange={(v) => setAction(v === ALL || !v ? "" : String(v))}
          >
            <SelectTrigger size="sm" aria-label="What">
              <SelectValue>
                {(v) => (v === "token" ? "Access tokens only" : "Sends and tokens")}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>Sends and tokens</SelectItem>
              <SelectItem value="token">Access tokens only</SelectItem>
            </SelectContent>
          </Select>
          {/* The window is FilterChips, as on Automation's job runs and on
              Usage. "All" is spelled out in the URL: absent means the 30-day
              default. */}
          <FilterChips label="How far back" options={windows} value={days} onChange={setDays} />
        </div>
        <FilterBar label="Filter by outcome" options={statuses} value={status} onChange={setStatus} />
        {data?.retention_days ? (
          <p className="text-xs text-muted-foreground">
            {/* The window this page prints is set two nav groups away, and an
                administrator reading a log during an incident is exactly the
                person who decides it is too short. The sentence carries the
                route rather than leaving them to know where it lives. */}
            Kept for {data.retention_days} days.{" "}
            <Link to="/admin/integrations#log" className="underline underline-offset-2 hover:text-foreground">
              Change on Integrations
            </Link>
            . Bodies aren’t stored unless body retention is on.
          </p>
        ) : null}
      </div>

      {isLoading ? (
        <Skeleton className="h-64 w-full" />
      ) : sends.length === 0 ? (
        // Three different empties. With any narrowing on, "nothing sent yet"
        // told an administrator the install had never sent anything.
        kind || action || status || recipient ? (
          <EmptyState
            title={recipient && !kind && !action && !status ? "Nothing sent to that address" : "No messages match"}
            action={
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  setKind("");
                  setAction("");
                  setStatus("");
                  setRecipient("");
                  setDays(ALL);
                }}
              >
                Clear
              </Button>
            }
          />
        ) : days !== ALL ? (
          <EmptyState
            icon={<Send className="h-6 w-6" />}
            title={`Nothing sent in the last ${windows.find((w) => w.value === days)?.label ?? `${days} days`}`}
            action={
              <Button variant="outline" size="sm" onClick={() => setDays(ALL)}>
                Show all time
              </Button>
            }
          />
        ) : (
          <EmptyState
            icon={<Send className="h-6 w-6" />}
            title="Nothing sent yet"
            description="Messages apps send through the Toft SDK appear here."
          />
        )
      ) : (
        <TableCard>
          {capped && (
            <p className="border-b border-border/60 px-4 py-2 text-xs text-muted-foreground">
              Showing the {FEED_LIMIT} most recent. Narrow the filters to see older messages.
            </p>
          )}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-8" />
                <TableHead>When</TableHead>
                <TableHead>App</TableHead>
                {/* Column priority: when, which app, did it arrive. The
                    detail row carries the rest. */}
                <TableHead className="hidden @xl:table-cell">Channel</TableHead>
                <TableHead className="hidden @4xl:table-cell">Went to</TableHead>
                <TableHead className="hidden @3xl:table-cell">Subject</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {sends.map((send) => (
                <SendRow key={send.id} send={send} titles={data?.kind_titles} />
              ))}
            </TableBody>
          </Table>
        </TableCard>
      )}
    </div>
  );
}

function SendRow({ send, titles }: { send: IntegrationSend; titles?: Record<string, string> }) {
  const [open, setOpen] = useState(false);
  // Recipients where there are any — mail is the one kind that has them — and
  // the destination the ledger recorded otherwise. Recorded before the send was
  // attempted, so it names where a message was going even if it never got there.
  const went =
    send.recipients.length > 0
      ? send.recipients.join(", ")
      : send.destination || "the default";
  const appLabel = send.app_name || send.app_slug || send.app_id.slice(0, 8);

  return (
    <Fragment>
      <TableRow>
        <TableCell className="pr-0">
          {/* Named after its row. Every expander on the page said "Show
              detail", so a reader tabbing the table met forty identical
              controls and had no way to tell which send each one opened — the
              rule the `⋯` menus already keep. */}
          <Button
            variant="ghost"
            size="icon"
            className="h-6 w-6"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
            aria-label={`${open ? "Hide" : "Show"} what ${appLabel} sent to ${went}`}
          >
            {open ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
          </Button>
        </TableCell>
        <TableCell className="whitespace-nowrap">
          <RelativeTime iso={send.created_at} />
        </TableCell>
        <TableCell>
          {/* The app's name, as every other admin table prints it, with the
              slug under it for matching against an address. */}
          <Link to={`/apps/${send.app_id}`} className="text-sm hover:underline">
            {appLabel}
          </Link>
          {send.app_name && send.app_slug && (
            <div className="font-mono text-2xs text-muted-foreground">{send.app_slug}</div>
          )}
        </TableCell>
        <TableCell className="hidden text-sm @xl:table-cell">
          {kindTitle(send.kind, titles)}
          {send.action === "file" ? " file" : send.action === "token" ? " access token" : ""}
        </TableCell>
        <TableCell className="hidden max-w-[18rem] truncate text-sm @4xl:table-cell">{went}</TableCell>
        <TableCell className="hidden max-w-[16rem] truncate text-sm @3xl:table-cell">
          {send.subject || <span className="text-muted-foreground">—</span>}
        </TableCell>
        <TableCell>
          <SendStatusBadge status={send.status} />
        </TableCell>
      </TableRow>

      {open && (
        <TableRow>
          <TableCell colSpan={7} className="bg-muted/30">
            <dl className="grid gap-x-8 gap-y-2 py-2 text-xs sm:grid-cols-2">
              <Detail label="Send ID" value={send.id} mono />
              <Detail label="Body size" value={formatBytes(send.body_bytes)} />
              {send.recipients.length > 0 && (
                <Detail
                  label={`Recipients (${send.recipients.length})`}
                  value={send.recipients.join(", ")}
                  className="sm:col-span-2"
                />
              )}
              {send.body_sha256 && (
                <Detail label="Body checksum" value={send.body_sha256} mono className="sm:col-span-2" />
              )}
              {send.attachments.length > 0 && (
                <Detail
                  label="Attachments"
                  value={send.attachments
                    .map((a) => `${a.name} (${formatBytes(a.bytes)}, ${a.type})`)
                    .join("; ")}
                  className="sm:col-span-2"
                />
              )}
              {send.error && (
                <Detail label="Why it wasn’t sent" value={send.error} className="sm:col-span-2" />
              )}
            </dl>
          </TableCell>
        </TableRow>
      )}
    </Fragment>
  );
}

function Detail({
  label,
  value,
  mono,
  className,
}: {
  label: string;
  value: string;
  mono?: boolean;
  className?: string;
}) {
  return (
    <div className={className}>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className={mono ? "break-all font-mono text-2xs" : "break-words"}>{value}</dd>
    </div>
  );
}
