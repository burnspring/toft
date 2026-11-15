import { Fragment, useState } from "react";
import { OUTCOME_LABEL, OUTCOME_TONE } from "@/lib/evidence-outcome";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Download, FileText } from "lucide-react";

import {
  api,
  type ComplianceControls,
  type ComplianceReport,
  type ComplianceRequirement,
  type ComplianceResult,
} from "@/lib/api";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { PageHeader } from "@/components/page-header";
import { TableCard } from "@/components/table-card";
import { QueryError } from "@/components/query-error";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { buttonVariants } from "@/components/ui/button-variants";
import { SearchInput } from "@/components/search-input";
import { EmptyState } from "@/components/empty-state";
import { FilterChips } from "@/components/filter-bar";
import { useUrlParam } from "@/hooks/use-list-state";
import { toneSurface } from "@/lib/tone";
import { RelativeTime } from "@/components/relative-time";
import { StatCard } from "@/components/stat-card";
import { Button } from "@/components/ui/button";
import { statColumns } from "@/lib/fact-grid";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { cn } from "@/lib/utils";
import { rowActivation } from "@/lib/row-activation";

/**
 * Admin → Governance → Compliance: what this install tells an assessor.
 *
 * Two documents from one registry, and the split between them is the page's
 * whole shape. The **control mapping** is a property of the build — what
 * Toft does about a requirement is the same sentence on every install of a
 * version — so it renders before anything is configured and can be circulated
 * once across an estate. The **live report** is a property of *this* install,
 * read now, and is the one that carries an assessment date.
 *
 * **No score.** No percentage, no overall verdict, no tile reading "83%
 * compliant". Nineteen of the ninety-seven requirements have nothing to do with
 * Toft, so any average mixes a platform control with a personnel one and
 * produces a figure whose only use is being quoted by somebody who has not read
 * the rest. The same argument the Reach page makes about severity, on a surface
 * where the temptation is stronger.
 *
 * The responsibility split leads instead, all four values at once, because the
 * two that claim nothing are what make the twenty-one that claim something
 * credible.
 */
export function AdminCompliancePage() {
  const controls = useQuery<ComplianceControls>({
    queryKey: ["compliance-controls"],
    queryFn: () => api.getComplianceControls(),
  });
  const report = useQuery<ComplianceReport>({
    queryKey: ["compliance-report"],
    queryFn: () => api.getComplianceReport(),
  });

  const coverage = controls.data?.coverage;
  // In the URL with the search and the responsibility chip, so a link to a
  // narrowed live report opens on the live report.
  const [tab, setTab] = useUrlParam("tab", "controls");

  return (
    <div>
      <PageHeader
        title="Compliance"
        description={
          coverage
            ? `NIST SP 800-171 Revision 3 — ${coverage.requirements} requirements across ${coverage.families} families.`
            : "NIST SP 800-171 Revision 3, mapped to Toft."
        }
        actions={<Downloads />}
      />

      {controls.error ? (
        <QueryError
          title="The control mapping could not be read"
          error={controls.error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void controls.refetch()}
        />
      ) : controls.isLoading || !controls.data ? (
        <TableCard className="space-y-2 p-4">
          {Array.from({ length: 8 }).map((_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </TableCard>
      ) : (
        <>
          <ResponsibilitySplit coverage={controls.data.coverage} />

          <Tabs value={tab} onValueChange={(v) => setTab(String(v))} className="mt-6">
            <TabsList>
              <TabsTrigger value="controls">
                Control mapping
                <Badge variant="secondary" className="ml-2">
                  {controls.data.coverage.requirements}
                </Badge>
              </TabsTrigger>
              <TabsTrigger value="report">
                Live report
                <Badge variant="secondary" className="ml-2">
                  {controls.data.coverage.evidence}
                </Badge>
              </TabsTrigger>
            </TabsList>

            <TabsContent value="controls">
              <ControlTable controls={controls.data} />
            </TabsContent>

            <TabsContent value="report">
              <LiveReport query={report} />
            </TabsContent>
          </Tabs>
        </>
      )}
    </div>
  );
}

/* ------------------------------------------------------------------ split */

/**
 * Who is answerable for a requirement, and what to call them.
 *
 * `key` is the server's enum and never rendered. `label` heads the tile;
 * `short` is the same fact where there is room for one or two words — the
 * filter chips and the table's own column, both of which used to print the raw
 * enum. "platform", "shared", "customer" is a schema detail, on the one page
 * in this product an outside assessor reads.
 */
const RESPONSIBILITY: { key: string; label: string; short: string; hint: string }[] = [
  {
    key: "platform",
    label: "Toft implements",
    short: "Toft",
    hint: "This install’s configuration is the evidence.",
  },
  {
    key: "shared",
    label: "Shared with you",
    short: "Shared",
    hint: "Toft provides the mechanism; you configure and run it.",
  },
  {
    key: "inherited",
    label: "Inherited below",
    short: "Inherited",
    hint: "The host, the cluster, the facility or the identity provider.",
  },
  {
    key: "customer",
    label: "Yours alone",
    short: "Yours",
    hint: "Toft has no part in it.",
  },
];

/** The word for a responsibility, never the enum. Falls back to the raw value
 *  so a family the server adds is still readable rather than blank. */
function responsibilityLabel(key: string): string {
  return RESPONSIBILITY.find((r) => r.key === key)?.short ?? key;
}

function ResponsibilitySplit({
  coverage,
}: {
  coverage: ComplianceControls["coverage"];
}) {
  return (
    // The one figure tile, four of it. No tone on any figure: every one of
    // these is a plain count of requirements; the split is the message, not a
    // verdict on any part of it.
    <div className={cn("grid grid-cols-1 gap-3", statColumns(RESPONSIBILITY.length))}>
      {RESPONSIBILITY.map((r) => (
        <StatCard
          key={r.key}
          label={r.label}
          value={coverage.by_responsibility[r.key] ?? 0}
          description={r.hint}
        />
      ))}
    </div>
  );
}

/* -------------------------------------------------------------- downloads */

/**
 * Both artifacts are plain links rather than fetch-and-blob.
 *
 * They are session-cookie GETs on the same origin, so the browser's own
 * download does the right thing and gets the server's filename from
 * `Content-Disposition` — which names the install and the day for the export,
 * and the build for the PDF. Fetching them into memory to re-offer them would
 * throw that away and buy nothing.
 */
function Downloads() {
  return (
    <>
      <a
        href="/api/v1/admin/compliance/controls.pdf"
        className={buttonVariants({ variant: "outline", size: "sm" })}
      >
        <FileText className="h-4 w-4" />
        Control mapping (PDF)
      </a>
      <a
        href="/api/v1/admin/compliance/report/export"
        className={buttonVariants({ variant: "outline", size: "sm" })}
      >
        <Download className="h-4 w-4" />
        Export live report
      </a>
    </>
  );
}

/* --------------------------------------------------------------- controls */

function ControlTable({ controls }: { controls: ComplianceControls }) {
  // Both in the URL. This is the one page in the product an outside assessor
  // reads over somebody's shoulder, and "the eleven requirements that are
  // yours alone" was a state that could be reached and not sent.
  const [q, setQ] = useUrlParam("q");
  const [resp, setResp] = useUrlParam("responsibility");
  const [openFamilies, setOpenFamilies] = useState<Set<string>>(() => new Set());

  const needle = q.trim().toLowerCase();
  // The search first, so the chips' counts are of what the search left —
  // counting the whole catalogue put "All 120" over a table of four.
  const searched = controls.requirements.filter((r) => {
    if (!needle) return true;
    return (
      r.id.toLowerCase().includes(needle) ||
      r.title.toLowerCase().includes(needle) ||
      r.family_title.toLowerCase().includes(needle) ||
      r.summary.toLowerCase().includes(needle)
    );
  });
  const rows = resp ? searched.filter((r) => r.responsibility === resp) : searched;

  // Under their families, in catalogue order. Ninety-seven rows with the
  // family printed on each was a 5,700px page with no outline; seventeen
  // headings are the outline, and a family opens to its requirements. Closed
  // by default on the whole catalogue, open while a search or a chip has
  // narrowed it — a narrowed list is short, and hiding the matches behind a
  // heading would make the search look as if it found nothing.
  const families: { title: string; rows: ComplianceRequirement[] }[] = [];
  for (const r of rows) {
    const last = families[families.length - 1];
    if (last && last.title === r.family_title) last.rows.push(r);
    else families.push({ title: r.family_title, rows: [r] });
  }
  const narrowed = needle !== "" || resp !== "";
  const isOpen = (title: string) => narrowed || openFamilies.has(title);
  const toggleFamily = (title: string) =>
    setOpenFamilies((prev) => {
      const next = new Set(prev);
      if (next.has(title)) next.delete(title);
      else next.add(title);
      return next;
    });

  return (
    <div className="mt-4">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <SearchInput
          value={q}
          onChange={setQ}
          placeholder="Search by requirement, title or family…"
          className="w-full sm:max-w-sm"
        />
        <FilterChips
          label="Filter by responsibility"
          options={[
            { label: "All", value: "", count: searched.length },
            ...RESPONSIBILITY.map((r) => ({
              label: r.short,
              value: r.key,
              count: searched.filter((x) => x.responsibility === r.key).length,
            })),
          ]}
          value={resp}
          onChange={setResp}
        />
      </div>

      {rows.length === 0 ? (
        <EmptyState
          title="No requirement matches"
          description="Clear the search or choose All."
          action={
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                setQ("");
                setResp("");
              }}
            >
              Clear
            </Button>
          }
        />
      ) : (
      <TableCard>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-[6.5rem]">Requirement</TableHead>
              <TableHead className="w-full">Title</TableHead>
              <TableHead className="hidden w-[8rem] @2xl:table-cell">
                Responsibility
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {families.map((fam) => (
              <Fragment key={fam.title}>
                <TableRow
                  {...rowActivation(
                    `${isOpen(fam.title) ? "Close" : "Open"} ${fam.title}, ${fam.rows.length} requirements`,
                    () => toggleFamily(fam.title),
                  )}
                  aria-expanded={isOpen(fam.title)}
                  className="bg-muted/40 hover:bg-muted/60"
                >
                  <TableCell colSpan={3} className="py-2">
                    <span className="inline-flex items-center gap-2 text-sm font-semibold text-foreground">
                      <ChevronRight
                        className={cn("h-3.5 w-3.5 transition-transform", isOpen(fam.title) && "rotate-90")}
                        aria-hidden
                      />
                      {fam.title}
                      <span className="text-xs font-normal text-muted-foreground tabular-nums">
                        {fam.rows.length}
                      </span>
                    </span>
                  </TableCell>
                </TableRow>
                {isOpen(fam.title) && fam.rows.map((r) => <RequirementRow key={r.id} r={r} />)}
              </Fragment>
            ))}
          </TableBody>
        </Table>
      </TableCard>
      )}
    </div>
  );
}

function RequirementRow({ r }: { r: ComplianceRequirement }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      {/* The row is the only control: there is no link inside it and nothing
          else opens the detail, so a bare `<tr onClick>` put every
          requirement's text and every piece of evidence out of reach of a
          keyboard — on the one page in this product built to be read by an
          assessor. */}
      <TableRow
        {...rowActivation(
          `${open ? "Hide" : "Show"} the detail of ${r.id}: ${r.title}`,
          () => setOpen((v) => !v),
        )}
        aria-expanded={open}
      >
        <TableCell className="font-mono text-xs tabular-nums">{r.id}</TableCell>
        {/* max-w-0 so a long title truncates rather than setting the column’s
            floor; TableCell is whitespace-nowrap. */}
        <TableCell className="max-w-0">
          <div className="truncate font-medium">{r.title}</div>
          <div className="truncate text-xs text-muted-foreground">
            {r.summary}
          </div>
        </TableCell>
        {/* Neutral for all four: who answers for a requirement is a fact
            about it, not a state, and a green "Toft" beside the live report's
            outcome chips read as "passing". */}
        <TableCell className="hidden @2xl:table-cell">
          <span className={cn("rounded px-1.5 py-0.5 text-2xs font-medium", toneSurface.neutral)}>
            {responsibilityLabel(r.responsibility)}
          </span>
        </TableCell>
      </TableRow>

      {open && (
        <TableRow>
          <TableCell colSpan={3} className="whitespace-normal bg-muted/30 p-4">
            <RequirementDetail r={r} />
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

function RequirementDetail({ r }: { r: ComplianceRequirement }) {
  return (
    <div className="space-y-4 text-sm">
      <section>
        <h3 className="text-sm font-semibold text-foreground">
          What NIST requires
        </h3>
        <ul className="mt-1 space-y-1">
          {r.statement.map((s, i) => (
            <li
              key={i}
              className="text-muted-foreground"
              style={{ marginLeft: `${s.depth}rem` }}
            >
              {s.label && (
                <span className="mr-1 font-mono text-2xs">{s.label}</span>
              )}
              {s.prose}
            </li>
          ))}
        </ul>
      </section>

      {r.mechanism && (
        <section>
          <h3 className="text-sm font-semibold text-foreground">
            How Toft addresses it
          </h3>
          <p className="mt-1 text-muted-foreground">{r.mechanism}</p>
        </section>
      )}

      {r.customer && (
        <section>
          <h3 className="text-sm font-semibold text-foreground">
            What you must still do
          </h3>
          <p className="mt-1 text-muted-foreground">{r.customer}</p>
        </section>
      )}

      {r.odps && r.odps.length > 0 && (
        <section>
          <h3 className="text-sm font-semibold text-foreground">
            Organization-defined parameters
          </h3>
          <ul className="mt-1 space-y-1">
            {r.odps.map((o) => (
              <li key={o.id} className="text-muted-foreground">
                <span className="mr-2 font-mono text-2xs">{o.id}</span>
                {o.guideline ?? o.usage}
              </li>
            ))}
          </ul>
        </section>
      )}

      {r.refs && r.refs.length > 0 && (
        <p className="text-xs text-muted-foreground">
          Written down in Toft:{" "}
          <span className="font-mono">{r.refs.join(" · ")}</span>
        </p>
      )}
    </div>
  );
}

/* ------------------------------------------------------------ live report */

// OUTCOME_TONE and OUTCOME_LABEL are lib/evidence-outcome.ts's, shared with
// the app's audit file.

function LiveReport({
  query,
}: {
  query: ReturnType<typeof useQuery<ComplianceReport>>;
}) {
  if (query.error) {
    return (
      <QueryError
        title="The live report could not be read"
        error={query.error}
        fallback={ADMIN_ONLY_FALLBACK}
        onRetry={() => void query.refetch()}
      />
    );
  }
  if (query.isLoading || !query.data) {
    return (
      <TableCard className="mt-4 space-y-2 p-4">
        {Array.from({ length: 6 }).map((_, i) => (
          <Skeleton key={i} className="h-10" />
        ))}
      </TableCard>
    );
  }

  const rep = query.data;
  return (
    <div className="mt-4">
      <p className="text-sm text-muted-foreground">
        “Capability absent” means this install doesn’t offer it — not a failure.
      </p>

      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
        <span>
          Collected{" "}
          <RelativeTime iso={rep.collected_at} exact />
        </span>
        {rep.hostname && (
          <span>
            Install <span className="font-mono">{rep.hostname}</span>
          </span>
        )}
        {rep.version && (
          <span>
            Version <span className="font-mono">{rep.version}</span>
          </span>
        )}
        {Object.entries(rep.by_outcome).map(([outcome, n]) => (
          <span
            key={outcome}
            className={cn(
              "rounded px-1.5 py-0.5 text-2xs font-medium",
              toneSurface[OUTCOME_TONE[outcome] ?? "neutral"],
            )}
          >
            {OUTCOME_LABEL[outcome] ?? outcome} {n}
          </span>
        ))}
      </div>

      <TableCard className="mt-3">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-full">Evidence</TableHead>
              <TableHead className="w-[9rem]">Outcome</TableHead>
              <TableHead className="hidden @3xl:table-cell">Source</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rep.results.map((row) => (
              <EvidenceRow key={row.id} row={row} />
            ))}
          </TableBody>
        </Table>
      </TableCard>
    </div>
  );
}

function EvidenceRow({ row }: { row: ComplianceResult }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <TableRow
        {...rowActivation(
          `${open ? "Hide" : "Show"} the evidence for ${row.label}`,
          () => setOpen((v) => !v),
        )}
        aria-expanded={open}
      >
        <TableCell className="max-w-0">
          <div className="truncate font-medium">{row.label}</div>
          {row.reason && (
            <div className="truncate text-xs text-muted-foreground">
              {row.reason}
            </div>
          )}
        </TableCell>
        <TableCell>
          <span
            className={cn(
              "rounded px-1.5 py-0.5 text-2xs font-medium",
              toneSurface[OUTCOME_TONE[row.outcome] ?? "neutral"],
            )}
          >
            {OUTCOME_LABEL[row.outcome] ?? row.outcome}
          </span>
        </TableCell>
        <TableCell className="hidden font-mono text-2xs text-muted-foreground @3xl:table-cell">
          {row.route ?? row.screen ?? "—"}
        </TableCell>
      </TableRow>

      {open && (
        <TableRow>
          <TableCell colSpan={3} className="whitespace-normal bg-muted/30 p-4">
            {row.reading && (
              <p className="mb-2 text-sm text-muted-foreground">{row.reading}</p>
            )}
            {row.value !== undefined && row.value !== null && (
              <pre className="max-h-72 overflow-auto rounded border border-border/60 bg-card p-3 font-mono text-2xs">
                {JSON.stringify(row.value, null, 2)}
              </pre>
            )}
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

export default AdminCompliancePage;
