import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Download } from "lucide-react";

import { api, type AppAuditFileSection } from "@/lib/api";
import { buttonVariants } from "@/components/ui/button-variants";
import { Badge } from "@/components/ui/badge";
import { QueryError } from "@/components/query-error";
import { Disclosure } from "@/components/disclosure";
import { Fact, Section } from "@/components/section";
import { SectionRail } from "@/components/section-rail";
import { useSectionSpy } from "@/hooks/use-section-spy";
import { humanize } from "@/lib/humanize";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { RelativeTime } from "@/components/relative-time";
import { OUTCOME_LABEL, OUTCOME_TONE } from "@/lib/evidence-outcome";
import { formatDateTime } from "@/lib/time";
import { toneSurface } from "@/lib/tone";

/**
 * An app's Audit tab: its audit file, section by section, and its export.
 *
 * What an editor hands an assessor about this one app. Each section is
 * collected as the route it mirrors would answer this reader, so a section
 * they may not have is shown **refused, with why** — never hidden, because a
 * pack with a section missing reads as complete. Nothing here is editable.
 */
export function AuditTab({ appId }: { appId: string }) {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["app-audit-file", appId],
    queryFn: () => api.getAppAuditFile(appId),
    retry: false,
  });

  if (isLoading) {
    return (
      <div className="space-y-3 pt-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-24 w-full" />
        ))}
      </div>
    );
  }
  if (error || !data) {
    return (
      <div className="pt-4">
        <QueryError
          title="The audit file could not be read"
          error={error}
          fallback="Only people who can edit this app can read its audit file."
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  // The longest page in the product, so it gets the rail Settings and Access
  // have, beside the same 3xl column. Built from the response: the sections
  // are the server's closed list, and every one of them renders.
  const railSections = [
    { id: "audit-file", title: "Audit file" },
    ...data.sections.map((sec) => ({ id: `audit-${sec.id}`, title: sec.label })),
  ];
  return (
    <AuditLayout sections={railSections}>
      <Section
        id="audit-file"
        title="Audit file"
        description="Evidence for an assessment of this app, not a passed audit."
        actions={
          <a
            href={`/api/v1/apps/${appId}/audit-file/export`}
            className={buttonVariants({ variant: "ghost", size: "sm" })}
          >
            <Download className="mr-2 h-3.5 w-3.5" /> Export
          </a>
        }
      >
        <div className="space-y-3 p-4 text-sm">
          {/* Two muted lines rather than a paragraph and an info Notice: a
              Notice is for a state, and "reviewing access is yours to do" is
              background that holds on every app, every time. */}
          <p className="text-muted-foreground">
            Read <RelativeTime iso={data.collected_at} exact />. The export adds a digest anyone can
            recompute; this install keeps no copy.
          </p>
          <p className="text-muted-foreground">
            It lists who has access and who granted it. Whether each grant should stay is your
            access review’s call.
          </p>
        </div>
      </Section>

      {data.sections.map((sec) => (
        <AuditSection key={sec.id} section={sec} />
      ))}
    </AuditLayout>
  );
}

function AuditLayout({
  sections,
  children,
}: {
  sections: { id: string; title: string }[];
  children: React.ReactNode;
}) {
  const [active, setActive] = useState("");
  useSectionSpy(
    sections.map((s) => s.id),
    setActive,
  );
  return (
    <div className="flex max-w-[61.5rem] gap-10 pt-4">
      <div className="min-w-0 max-w-3xl flex-1 space-y-4">{children}</div>
      <SectionRail sections={sections} active={active} label="Audit file sections" />
    </div>
  );
}

function AuditSection({ section }: { section: AppAuditFileSection }) {
  const tone = OUTCOME_TONE[section.outcome] ?? "neutral";
  const structured = section.value !== null && typeof section.value === "object";
  return (
    <Section
      id={`audit-${section.id}`}
      title={section.label}
      actions={
        <Badge variant="outline" className={`text-xs font-medium ${toneSurface[tone]}`}>
          {OUTCOME_LABEL[section.outcome] ?? section.outcome}
        </Badge>
      }
    >
      <div className="space-y-4 p-4">
        {section.outcome === "ok" ? (
          <>
            <AuditValue value={section.value} depth={0} />
            {/* The exact shape the export carries, for the reader who has to
                match it field by field. A string section has no other shape. */}
            {structured && (
              <Disclosure label="Raw JSON" size="xs">
                <pre
                  tabIndex={0}
                  role="region"
                  aria-label={`${section.label}, raw JSON`}
                  className="max-h-80 overflow-auto rounded-md bg-muted/50 p-3 font-mono text-xs whitespace-pre-wrap text-muted-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none"
                >
                  {JSON.stringify(section.value, null, 2)}
                </pre>
              </Disclosure>
            )}
          </>
        ) : (
          <p className="text-sm text-muted-foreground">{section.reason}</p>
        )}
      </div>
    </Section>
  );
}

/*
 * A section's value, rendered by its shape rather than per section: the list of
 * sections is closed (ADR §93) but each one's fields move with the route it
 * mirrors, and a renderer keyed by field name would fall silently behind.
 *
 *   - a string is prose;
 *   - an object's scalar fields are a facts grid, its nested fields a titled
 *     group under it — down to MAX_DEPTH, past which a value prints compact;
 *   - an array of objects is a table, an array of scalars a list.
 *
 * Nothing is dropped: every field the response carries is on screen somewhere,
 * and the Raw JSON disclosure beside it is the exact shape.
 */
const MAX_DEPTH = 3;

type Json = string | number | boolean | null | Json[] | { [k: string]: Json };

function isRecord(v: unknown): v is Record<string, Json> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

function isScalar(v: unknown): boolean {
  return v === null || v === undefined || typeof v !== "object";
}

function AuditValue({ value, depth }: { value: unknown; depth: number }) {
  if (Array.isArray(value)) return <AuditList items={value as Json[]} depth={depth} />;
  if (isRecord(value)) {
    // A record of like records — `rows_left`, keyed by table — is a table with
    // its keys as the first column, not a grid of run-on sentences. The key is
    // kept as written and set in mono: an assessor matches it against the export.
    const inner = Object.values(value);
    if (
      inner.length >= 2 &&
      inner.every((v) => isRecord(v) && Object.values(v).every(isScalar))
    ) {
      const rows = Object.entries(value).map(([k, v]) => ({ "": k, ...(v as Record<string, Json>) }));
      return <AuditList items={rows} depth={depth} />;
    }
    return <AuditObject value={value} depth={depth} />;
  }
  if (typeof value === "string" && value.length > 60) {
    return <p className="max-w-prose text-sm text-foreground">{value}</p>;
  }
  return <span className="text-sm">{<Scalar value={value as Json} />}</span>;
}

function AuditObject({ value, depth }: { value: Record<string, Json>; depth: number }) {
  const entries = Object.entries(value);
  if (entries.length === 0) return <None />;
  // A nested field that is itself a flat little record (`created_by`) reads as
  // one fact; only one with structure of its own earns a group.
  const asFact = ([, v]: [string, Json]) =>
    isScalar(v) || depth >= MAX_DEPTH || (isRecord(v) && Object.values(v).every(isScalar) && Object.keys(v).length <= 3);
  const facts = entries.filter(asFact);
  const groups = entries.filter((e) => !asFact(e));
  return (
    <div className="space-y-4">
      {facts.length > 0 && (
        <dl className="grid gap-x-8 gap-y-3 sm:grid-cols-2 lg:grid-cols-3">
          {facts.map(([k, v]) => (
            // A sentence takes the row; a grid cell would wrap it to a column of words.
            <div key={k} className={typeof v === "string" && v.length > 60 ? "sm:col-span-full" : undefined}>
              <Fact label={humanize(k)}>
                <Cell value={v} />
              </Fact>
            </div>
          ))}
        </dl>
      )}
      {groups.map(([k, v]) => (
        // Below the top level a group is ruled at its left, or Serving inside
        // State reads as State's sibling.
        <div key={k} className={depth > 0 ? "space-y-2 border-l border-border/60 pl-4" : "space-y-2"}>
          <h4 className="text-xs font-medium text-muted-foreground">{humanize(k)}</h4>
          <AuditValue value={v} depth={depth + 1} />
        </div>
      ))}
    </div>
  );
}

function AuditList({ items, depth }: { items: Json[]; depth: number }) {
  if (items.length === 0) return <None />;
  if (items.every(isRecord)) {
    const rows = items as Record<string, Json>[];
    const columns = [...new Set(rows.flatMap((r) => Object.keys(r)))];
    return (
      <div className="overflow-x-auto rounded-md border border-border/60">
        <Table>
          <TableHeader>
            <TableRow>
              {columns.map((c) => (
                <TableHead key={c} className="text-xs whitespace-nowrap">
                  {humanize(c)}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r, i) => (
              <TableRow key={i}>
                {columns.map((c) => (
                  <TableCell key={c} className="h-auto py-2 align-top text-xs whitespace-nowrap">
                    {c === "" ? (
                      <span className="font-mono">{String(r[c])}</span>
                    ) : (
                      <Cell value={r[c] ?? null} inTable />
                    )}
                  </TableCell>
                ))}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    );
  }
  if (items.every(isScalar)) {
    return (
      <ul className="max-w-prose list-disc space-y-1 pl-5 text-sm">
        {items.map((v, i) => (
          <li key={i}>
            <Scalar value={v} />
          </li>
        ))}
      </ul>
    );
  }
  // Mixed or nested arrays: each item on its own, one level down.
  return (
    <div className="space-y-3">
      {items.map((v, i) => (
        <AuditValue key={i} value={v} depth={depth + 1} />
      ))}
    </div>
  );
}

/** A value in a fact or a table cell — never a nested layout. */
function Cell({ value, inTable = false }: { value: Json; inTable?: boolean }) {
  if (isScalar(value)) return <Scalar value={value} inTable={inTable} />;
  if (Array.isArray(value)) {
    if (value.length === 0) return <None />;
    if (value.every(isScalar)) return <>{value.map((v) => String(v)).join(", ")}</>;
    return <Compact value={value} inTable={inTable} />;
  }
  const r = value as Record<string, Json>;
  if (Object.keys(r).length === 0) return <None />;
  // A principal or a reference is read by its name.
  for (const k of ["display_name", "name", "login", "label"]) {
    if (typeof r[k] === "string" && r[k] !== "") return <>{r[k]}</>;
  }
  if (Object.values(r).every(isScalar)) {
    return (
      <>
        {Object.entries(r).map(([k, v], i) => (
          <span key={k}>
            {i > 0 && <span className="text-muted-foreground"> · </span>}
            <span className="text-muted-foreground">{humanize(k)}</span> <Scalar value={v} />
          </span>
        ))}
      </>
    );
  }
  return <Compact value={r} inTable={inTable} />;
}

function Compact({ value, inTable = false }: { value: Json; inTable?: boolean }) {
  const s = JSON.stringify(value);
  if (inTable) return <Clipped text={s} className="font-mono text-muted-foreground" />;
  return <code className="font-mono text-xs break-all text-muted-foreground">{s}</code>;
}

/** One line in a table cell, the whole text on hover and in Raw JSON. A row
 *  that wraps a user agent and four ids grew to a screen of its own. */
function Clipped({ text, className }: { text: string; className?: string }) {
  return (
    <span title={text} className={`block max-w-64 truncate ${className ?? ""}`}>
      {text}
    </span>
  );
}

const ISO_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/;
const IDENTIFIER = /^[0-9a-f]{8}-[0-9a-f]{4}-|^[0-9a-f]{32,}$/i;

function Scalar({ value, inTable = false }: { value: Json | undefined; inTable?: boolean }) {
  if (value === null || value === undefined || value === "") return <None />;
  if (typeof value === "boolean") return <>{value ? "Yes" : "No"}</>;
  if (typeof value === "number") return <span className="tabular-nums">{value.toLocaleString()}</span>;
  const s = String(value);
  // Exact, not relative: an assessor compares these against other records.
  if (ISO_TIME.test(s)) return <time dateTime={s}>{formatDateTime(s)}</time>;
  if (IDENTIFIER.test(s)) {
    // In a table an id is for telling rows apart; its first eight say that.
    if (inTable) {
      return (
        <span title={s} className="font-mono text-xs">
          {s.slice(0, 8)}…
        </span>
      );
    }
    return <span className="font-mono text-xs break-all">{s}</span>;
  }
  if (inTable && s.length > 48) return <Clipped text={s} />;
  return <>{s}</>;
}

function None() {
  return <span className="text-muted-foreground">None</span>;
}

