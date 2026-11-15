import { Box, Layers, UserRound, Users } from "lucide-react";
import { tagColorClass } from "@/lib/tags";
import { executionModeLabel, runningMode } from "@/lib/execution-mode";
import type { FacetOption } from "@/components/facet-filter";
import { runtimeSeriesKey, runtimeSeriesLabel } from "@/lib/runtime-series";
import type { AppRuntime, AppTag, AppWithMetrics, ExecutionMode } from "@/lib/api";
import { cn } from "@/lib/utils";

/**
 * How the apps list's facets are derived — every one of them, in one file.
 *
 * All of these read the apps **already loaded** rather than fetching a
 * vocabulary of their own. Two reasons, and the second is the important one:
 * the listing carries every fact these need anyway, and a separately-fetched
 * vocabulary can offer a value that narrows the visible list to nothing, which
 * reads as a broken filter rather than an empty result.
 */

/** The key a tag pair filters by. Also its wire form in the URL. */
export function tagFacetKey(t: Pick<AppTag, "tag_slug" | "value_slug">) {
  return `${t.tag_slug}:${t.value_slug}`;
}

export function tagFacetOptions(apps: AppWithMetrics[]): FacetOption[] {
  const byTag = new Map<string, { name: string; values: Map<string, AppTag> }>();
  for (const app of apps) {
    for (const t of app.tags ?? []) {
      let group = byTag.get(t.tag_slug);
      if (!group) {
        group = { name: t.tag_name, values: new Map() };
        byTag.set(t.tag_slug, group);
      }
      group.values.set(t.value_slug, t);
    }
  }
  return [...byTag.values()]
    .sort((a, b) => a.name.localeCompare(b.name))
    .flatMap((group) =>
      [...group.values.values()]
        .sort((a, b) => a.value.localeCompare(b.value))
        .map((t): FacetOption => ({
          key: tagFacetKey(t),
          label: t.value,
          group: group.name,
          prefix: `${t.tag_name}:`,
          chipClassName: tagColorClass(t.color),
          marker: (
            <span className={cn("h-2 w-2 rounded-full border", tagColorClass(t.color))} />
          ),
        })),
    );
}

const modeIcon = { shared: Layers, isolated: Box } as const;

/**
 * The modes actually present in this list, never both by default.
 *
 * An install that runs everything one way has one mode, so it gets no facet:
 * a filter offering a single value narrows nothing. This decides the **filter
 * only** — the Mode column is gated by `anyIsolated`, asymmetrically and for a
 * different reason. Counting distinct modes answers the filter's question and
 * was never the column's, and reading it as both is what hid the column on an
 * install where every app is isolated.
 *
 * A mode an app is only *set* to counts as present: an administrator who
 * promoted an app and has not redeployed it yet is exactly the person who needs
 * to find it again.
 */
export function modeFacetOptions(apps: AppWithMetrics[]): FacetOption[] {
  // `runningMode` only, and deliberately not `execution_mode`: the filter
  // beside this narrows on where an app *runs*, so offering the mode an app is
  // merely set to move to on its next deploy put an option in the popover that
  // could only ever select nothing — the exact failure a derived vocabulary
  // exists to prevent. The Mode column still says both, because a column is a
  // statement about a row rather than a control.
  const present = new Set<ExecutionMode>();
  // A row held at the `list` rung carries no mode, and `runningMode` would
  // read its absence as shared.
  for (const app of apps) if (app.can_open) present.add(runningMode(app));
  return (["shared", "isolated"] as const)
    .filter((m) => present.has(m))
    .map((m): FacetOption => {
      const Icon = modeIcon[m];
      return {
        key: m,
        label: executionModeLabel[m],
        prefix: "Mode:",
        marker: <Icon className="h-3 w-3 opacity-60" />,
      };
    });
}

/**
 * The release lines present in this list — Python 3.13, Node 22, Go unresolved.
 *
 * Derived from the apps already loaded, like the two above, and for the same
 * reason: this is the vocabulary that can actually narrow the list. It is also
 * the axis the census drills through to, so its keys are
 * `runtimeSeriesKey`'s — one definition, used by the link that arrives and the
 * filter that receives it.
 *
 * Absent for anybody but an administrator, because the server does not send
 * `runtime` to anybody else. That is not this file's decision to make and it
 * needs no branch here: no runtime on the rows means no options, and a facet
 * with no options renders nothing.
 *
 * Ordered newest series first within a language, with the two wordless states
 * last — an app whose version nobody recorded is not on a release line and must
 * not sort among them.
 */
export function versionFacetOptions(apps: AppWithMetrics[]): FacetOption[] {
  const byKey = new Map<string, AppRuntime>();
  for (const app of apps) {
    if (app.runtime) byKey.set(runtimeSeriesKey(app.runtime), app.runtime);
  }
  return [...byKey.entries()]
    .sort(([, a], [, b]) => compareRuntime(a, b))
    .map(([key, r]): FacetOption => ({
      key,
      label: runtimeSeriesLabel(r),
      group: r.label,
      prefix: `${r.label}:`,
      search: `${r.label} ${r.series}`,
    }));
}

function compareRuntime(a: AppRuntime, b: AppRuntime): number {
  if (a.label !== b.label) return a.label.localeCompare(b.label);
  if ((a.state === "resolved") !== (b.state === "resolved")) {
    return a.state === "resolved" ? -1 : 1;
  }
  if (a.state !== "resolved") return a.state.localeCompare(b.state);
  const av = a.series.split(".").map(Number);
  const bv = b.series.split(".").map(Number);
  for (let i = 0; i < Math.max(av.length, bv.length); i++) {
    const d = (bv[i] ?? 0) - (av[i] ?? 0);
    if (d) return d;
  }
  return 0;
}

/** The key an app's owner filters by — `user:<id>` or `group:<id>` — and its
 *  wire form in the URL (`?owner=`). Typed, because a person and a group are
 *  different principals and their ids are not one namespace. */
export function ownerFacetKey(app: Pick<AppWithMetrics, "owner_id" | "owner_group_id">): string {
  return app.owner_group_id ? `group:${app.owner_group_id}` : `user:${app.owner_id}`;
}

/**
 * Who owns the apps in this list — the people and the groups, under a heading
 * each, because a group is not a person with an unusual name (`AppOwner`).
 *
 * Derived from the rows like the facets above, so it never offers an owner
 * whose apps are not on the page. A person is named as the Owner column names
 * them — the display name, or the login when the provider sent none — with the
 * login searchable either way. A group's name comes from the group read the
 * page already shares with `AppOwner`; a group this account cannot read the
 * name of keeps its place, as "Group", rather than vanishing from the filter.
 */
export function ownerFacetOptions(
  apps: AppWithMetrics[],
  groups: { id: string; name: string }[],
): FacetOption[] {
  const people = new Map<string, FacetOption>();
  const teams = new Map<string, FacetOption>();
  for (const app of apps) {
    const key = ownerFacetKey(app);
    if (app.owner_group_id) {
      if (teams.has(key)) continue;
      const name = groups.find((g) => g.id === app.owner_group_id)?.name ?? "Group";
      teams.set(key, {
        key, label: name, group: "Groups", prefix: "Owner:",
        marker: <Users className="h-3 w-3 opacity-60" />,
      });
    } else if (app.owner && !people.has(key)) {
      people.set(key, {
        key, label: app.owner.name || app.owner.login, group: "People", prefix: "Owner:",
        search: app.owner.login,
        marker: <UserRound className="h-3 w-3 opacity-60" />,
      });
    }
  }
  const byLabel = (a: FacetOption, b: FacetOption) => a.label.localeCompare(b.label);
  return [...[...people.values()].sort(byLabel), ...[...teams.values()].sort(byLabel)];
}
