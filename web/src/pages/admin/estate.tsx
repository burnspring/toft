import { Boxes, Layers, Languages as LanguagesIcon } from "lucide-react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, type EstateCensus, type EstateLanguage, type EstateSeries } from "@/lib/api";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { SectionHeading } from "@/components/section";
import { TableCard } from "@/components/table-card";
import { QueryError } from "@/components/query-error";
import { EmptyState } from "@/components/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { FrameworkBadge } from "@/components/framework-badge";
import { Notice } from "@/components/notice";
import {
  runtimeSeriesFullLabel, runtimeSeriesKey, runtimeSeriesLabel, runtimeStateExplanation,
} from "@/lib/runtime-series";
import { cn } from "@/lib/utils";
import { APP_KIND_LABEL } from "@/lib/frameworks";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

/**
 * Admin → Estate → Languages: what this estate is written in.
 *
 * Not the framework count on /tags/framework, which stays where it is and
 * answers a different question for a different reader — "show me the Flask apps
 * I can see". This page answers "what breaks when a series goes end-of-life",
 * which needs three things that page cannot give: every app rather than the
 * reader's, a language version rather than a framework name, and the crossing
 * with execution mode that makes "what is still shared, and what is it written
 * in" one question instead of two screens.
 *
 * **The coverage line leads.** A census returns a total and a total feels
 * exhaustive, so the sentence saying how much of the estate has a recorded
 * version sits above the table before any count is shown — never a footnote and
 * never a tooltip.
 */
export function AdminEstatePage() {
  // A section of another admin page rather than a page of its own (ADR §50):
  // the host wraps it in the anchor its old address redirects to.
  const Header = SectionHeading;
  const { data, isLoading, error, refetch } = useQuery<EstateCensus>({
    queryKey: ["estate-languages"],
    queryFn: () => api.getEstateLanguages(),
  });

  return (
    <div className="space-y-3">
      <Header
        title="Apps by kind and language"
        // The other half of one decision, in a different admin group: this
        // page counts what apps resolve to, System info lists what is
        // installed. In the description rather than a lone line over the table.
        description={
          coverageSentence(data)
        }
      />

      {/* Error before empty: "nothing has been deployed yet" is the wrong
          sentence to show somebody whose read simply failed. */}
      {error ? (
        <QueryError
          title="Languages could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      ) : isLoading ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </TableCard>
      ) : data && data.languages.length === 0 ? (
        <EmptyState
          size="inline"
          icon={<LanguagesIcon className="h-6 w-6" />}
          title="No apps yet"
          description="Each app appears here under its language once it’s created."
        />
      ) : data ? (
        <div className="space-y-4">
          {/* The output the coverage line exists for. Every app under
              "Unresolved" is a real answer and must not read as a broken page,
              so the page says why rather than leaving a column of blanks to be
              read as missing data. */}
          {data.apps_versioned === 0 && data.apps_total > 0 && (
            <Notice tone="neutral" size="sm" title="No app on this install has a recorded runtime version">
              Versions are recorded on each app’s next deploy.
            </Notice>
          )}

          <KindsTable census={data} />

          <TableCard>
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Language</TableHead>
                  <TableHead>Version</TableHead>
                  <TableHead className="text-right">Apps</TableHead>
                  {/* The second axis. Absent where this install offers one
                      mode: a census carrying "Isolated 0" answers the
                      question, but only where isolation is a thing that could
                      happen here. */}
                  {data.show_modes && (
                    <>
                      <TableHead className="hidden text-right @md:table-cell">Shared</TableHead>
                      <TableHead className="hidden text-right @md:table-cell">Isolated</TableHead>
                    </>
                  )}
                  {/* The framework axis is kept and demoted: "which apps are
                      Next.js" is a real question, it is just not the one an
                      end-of-life migration asks. */}
                  <TableHead className="hidden @3xl:table-cell">Frameworks</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.languages.map((lang) => (
                  lang.series.map((s, i) => (
                    <SeriesRow
                      key={`${lang.language}:${s.state}:${s.series}`}
                      lang={lang}
                      series={s}
                      first={i === 0}
                      last={i === lang.series.length - 1}
                      showModes={data.show_modes}
                    />
                  ))
                ))}
              </TableBody>
            </Table>
            {/* Each wordless state explained once under the table rather than
                as a footnote on every row that carries it — a paragraph inside
                a cell widens the Version column until the count it belongs to
                is half a table away from it. Only the states actually present:
                a legend for a row nobody has is a definition of nothing. */}
            <StateLegend census={data} />
          </TableCard>
        </div>
      ) : null}
    </div>
  );
}

/**
 * The sentence above the table, and it is written before the table is read.
 *
 * "84 apps, 61 with a recorded runtime version" is what stops the counts below
 * being taken for the whole estate in a meeting. Locked apps are named here too
 * — they are counted in every number on the page, because a retired app still
 * has a release with a version on it, and saying so once is better than a
 * parenthesis on every row.
 */
function coverageSentence(data?: EstateCensus): string {
  // Also the sentence for an install with no apps: "0 apps, 0 with a recorded
  // runtime version" above an empty state says the same nothing twice.
  if (!data || data.apps_total === 0) {
    return "What your apps are written in, by version.";
  }
  const apps = `${data.apps_total} app${data.apps_total === 1 ? "" : "s"}`;
  const locked = data.apps_locked > 0 ? `, ${data.apps_locked} locked` : "";
  return `${apps}, ${data.apps_versioned} with a recorded runtime version${locked}.`;
}

/**
 * The estate by what answers a request — a process, files, or nothing (a batch
 * app) — and, as a fourth row that overlaps the three, how many declare jobs.
 * Each count opens the apps list narrowed by `?kind=`, which has no control of
 * its own there, only the chip.
 */
function KindsTable({ census }: { census: EstateCensus }) {
  const rows: { key: keyof typeof APP_KIND_LABEL; apps: number; what: string }[] = [
    { key: "serving", apps: census.kinds.serving, what: "A process answers every request" },
    { key: "rendered", apps: census.kinds.rendered, what: "Files built once and served as they are" },
    { key: "batch", apps: census.kinds.batch, what: "Runs jobs and serves no pages" },
    { key: "jobs", apps: census.apps_with_jobs, what: "Declares at least one job, of any kind above" },
  ];
  return (
    <TableCard>
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Kind</TableHead>
            <TableHead className="text-right">Apps</TableHead>
            <TableHead className="hidden w-full @xl:table-cell">What it means</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow key={r.key}>
              <TableCell className={cn("whitespace-nowrap", r.key === "jobs" ? "text-muted-foreground" : "font-medium")}>
                {APP_KIND_LABEL[r.key]}
              </TableCell>
              <TableCell className="text-right font-medium tabular-nums">
                <Link
                  to={`/?scope=all&kind=${r.key}`}
                  className="hover:text-primary hover:underline"
                  aria-label={`Show the ${r.apps} app${r.apps === 1 ? "" : "s"}: ${APP_KIND_LABEL[r.key]}`}
                >
                  {r.apps}
                </Link>
              </TableCell>
              <TableCell className="hidden text-muted-foreground @xl:table-cell">{r.what}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableCard>
  );
}

function SeriesRow({
  lang,
  series,
  first,
  last,
  showModes,
}: {
  lang: EstateLanguage;
  series: EstateSeries;
  first: boolean;
  /** The last release line for this language — the only row of the group that
   *  keeps its rule, so the lines under one language read as one block. */
  last: boolean;
  showModes: boolean;
}) {
  return (
    // `border-b-0`, not `border-t-0`: rows in this table are separated by a
    // bottom border, so the class this used to carry styled an edge that was
    // never drawn and every release line was ruled off from the one above it.
    <TableRow className={cn(!last && "border-b-0")}>
      {/* Repeated on every row rather than spanned: a row of this table has to
          be readable on its own, in a screen reader and on a phone. The group
          is carried by weight instead. */}
      <TableCell className={cn("whitespace-nowrap", first ? "font-medium text-foreground" : "text-muted-foreground")}>
        {lang.label}
      </TableCell>
      <TableCell>
        <span
          className={cn(
            "font-mono text-sm",
            series.state === "resolved" ? "text-foreground" : "font-sans text-muted-foreground",
          )}
        >
          {runtimeSeriesLabel(series)}
        </span>
      </TableCell>
      {/* The count is the link, and it goes to the apps list rather than to a
          list of its own: that page already knows what an app row looks like,
          who owns it, what state it is in and what to do with it, and a second
          listing would be a second definition of all four to keep in step. */}
      <TableCell className="text-right font-medium tabular-nums">
        <Link
          to={`/?scope=all&runtime=${encodeURIComponent(runtimeSeriesKey({ language: lang.language, series: series.series, state: series.state }))}`}
          className="hover:text-primary hover:underline"
          aria-label={`Show the ${series.apps} app${series.apps === 1 ? "" : "s"} on ${runtimeSeriesFullLabel(lang.label, series)}`}
        >
          {series.apps}
        </Link>
      </TableCell>
      {showModes && (
        <>
          <TableCell className="hidden text-right tabular-nums text-muted-foreground @md:table-cell">
            <span className="inline-flex items-center gap-1.5">
              <Layers className="h-3 w-3 opacity-60" aria-hidden />
              {series.shared}
            </span>
          </TableCell>
          <TableCell className="hidden text-right tabular-nums text-muted-foreground @md:table-cell">
            <span className="inline-flex items-center gap-1.5">
              <Boxes className="h-3 w-3 opacity-60" aria-hidden />
              {series.isolated}
            </span>
          </TableCell>
        </>
      )}
      <TableCell className="hidden @3xl:table-cell">
        <span className="flex flex-wrap items-center gap-1.5">
          {series.frameworks.map((f) => (
            <span key={f.framework} className="inline-flex items-center gap-1">
              <FrameworkBadge framework={f.framework} />
              <span className="text-2xs tabular-nums text-muted-foreground">{f.apps}</span>
            </span>
          ))}
        </span>
      </TableCell>
    </TableRow>
  );
}

/** The wordless states this census actually contains, in the order they appear. */
function StateLegend({ census }: { census: EstateCensus }) {
  const present = [estateStates[0], estateStates[1]].filter((state) =>
    census.languages.some((l) => l.series.some((s) => s.state === state)),
  );
  if (present.length === 0) return null;
  return (
    <dl className="space-y-1 border-t border-border/60 px-4 py-3 text-2xs text-muted-foreground">
      {present.map((state) => (
        <div key={state} className="flex flex-wrap gap-x-1.5">
          <dt className="font-medium text-foreground">{runtimeSeriesLabel({ series: "", state })}</dt>
          <dd>{runtimeStateExplanation(state)}</dd>
        </div>
      ))}
    </dl>
  );
}

const estateStates = ["unresolved", "not_applicable"] as const;
