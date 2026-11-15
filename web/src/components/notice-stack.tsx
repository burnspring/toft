import { useState } from "react";
import { ChevronDown } from "lucide-react";

import { orderNotices, type StackedNotice } from "@/lib/notice-stack";

/**
 * Every banner about the app's state, above the tab strip, at most two at once.
 *
 * Seven conditions could produce one and they lived in two places: five above
 * the tabs and two inside Overview, so *where* a message about this app
 * appeared depended on which message it was — and on a 757-pixel Overview the
 * two that lived there pushed its entire content below the fold (ADR §53 D6).
 *
 * The ceiling is the other half. Four could stack, each `size="md"`, which put
 * the tab strip off the first screen; and a banner nobody can see past is a
 * banner nobody reads. So two show and the remainder collapse to one line that
 * says how many and opens them — which generalizes the malware/blocked-deploys
 * suppression that got this right once by hand, rather than adding an eighth
 * condition to that chain.
 *
 * What it deliberately does not do is decide *whether* a condition earns a
 * banner. That budget is set where each notice is built, and the argument for
 * it — a moderate transitive CVE with no fix is on nearly every Node app, so a
 * banner for it would be permanent, identical on forty apps, and scrolled past
 * within a week, after which the malware banner is scrolled past too — is
 * untouched by this. The order and the ceiling live in `lib/notice-stack.ts`,
 * because that is the part with a rule in it and `npm test` runs without a DOM.
 */
export function NoticeStack({
  notices,
}: {
  /**
   * One entry per condition, in the caller's deliberate order, with the ones
   * that did not fire left as `false`. Taking them falsy rather than filtered
   * is what lets the call site read as a list of `condition && {…}` — which is
   * where the order lives, and is worth keeping legible.
   */
  notices: (StackedNotice | false | undefined | null)[];
}) {
  const [expanded, setExpanded] = useState(false);
  const { shown, hidden } = orderNotices(notices, expanded);
  if (shown.length === 0) return null;

  return (
    <div className="space-y-3">
      {shown.map((n) => (
        <div key={n.key}>{n.node}</div>
      ))}
      {/* Reversible. It was a one-way `setExpanded(true)`, so opening seven
          notices to read one pushed the tab strip off the screen for the rest
          of the visit — the ceiling exists to keep it there, and there was no
          way back under it. */}
      {(hidden > 0 || expanded) && (
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          aria-expanded={expanded}
          className="flex items-center gap-1.5 rounded-sm text-sm text-muted-foreground transition-colors hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50"
        >
          <ChevronDown
            className={`h-3.5 w-3.5 transition-transform ${expanded ? "rotate-180" : ""}`}
          />
          {expanded
            ? "Fewer notices"
            : `${hidden} more ${hidden === 1 ? "notice" : "notices"} about this app`}
        </button>
      )}
    </div>
  );
}
