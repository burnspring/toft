import type { ReactNode } from "react";

import { cn } from "@/lib/utils";

/**
 * A short literal inside prose — an env var, a command, a header, an endpoint.
 *
 * Six shapes of the same chip were in use: `rounded bg-muted px-1`, the same
 * plus `py-0.5`, the same plus `text-2xs`, one on `bg-black/5`, and two more.
 * None of them was a decision; each was whatever the file beside it happened to
 * have. `font-mono` is the product's second voice — anything a person copies or
 * compares character by character — and the tint is what makes a fragment of it
 * a *thing* rather than a change of typeface mid-sentence.
 *
 * The size is inherited on purpose: a chip in `text-xs` copy is `text-xs`, and
 * a chip that named its own size was the reason the same word rendered at three
 * sizes on one page.
 */
export function Code({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <code className={cn("rounded bg-muted px-1 py-0.5 font-mono", className)}>{children}</code>
  );
}
