import { Bot, CalendarClock, User } from "lucide-react";

import type { RunOrigin } from "@/lib/automation";

const VIA: Record<"viewer" | "cli" | "ui", string> = {
  viewer: "in the app",
  cli: "CLI",
  ui: "Run now",
};

/**
 * Who started a run.
 *
 * The rule it enforces is an invariant rather than a formatting preference: **a
 * run with no resolved person never renders as one.** An app may start its own
 * work, and that must stay visibly different from somebody pressing a button; a
 * schedule firing at 03:00 must never acquire somebody's name. The mapping from
 * the run's columns to the origin lives in `lib/automation.ts`; the drawing
 * lives here, once.
 */
export function RunProvenance({ origin }: { origin: RunOrigin }) {
  if (origin.kind === "person") {
    return (
      <span className="flex items-center gap-1.5 text-xs">
        <User className="h-3.5 w-3.5 text-muted-foreground" />
        {origin.label}
        <span className="text-muted-foreground">· {VIA[origin.via]}</span>
      </span>
    );
  }
  if (origin.kind === "schedule") {
    return (
      <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <CalendarClock className="h-3.5 w-3.5" />
        Schedule
      </span>
    );
  }
  if (origin.kind === "app") {
    return (
      <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <Bot className="h-3.5 w-3.5" />
        The app itself
      </span>
    );
  }
  // A start the platform has no word for, or a person nobody could resolve.
  // Rendered as a gap rather than guessed at, since every guess here is a claim
  // about who did something.
  return <span className="text-xs text-muted-foreground">—</span>;
}
