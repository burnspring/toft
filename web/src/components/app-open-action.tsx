import { ExternalLink } from "lucide-react";
import { Link } from "react-router-dom";

import { buttonVariants } from "@/components/ui/button-variants";
import type { AppWithMetrics } from "@/lib/api";
import { appUrl } from "@/lib/app-url";
import { isBatchFramework } from "@/lib/frameworks";
import { wakeAheadProps } from "@/lib/wake-ahead";

/**
 * A list row's primary act on an app: **Open** it, or — for a row held at the
 * `list` rung, which leads to the access gate rather than the app — **Request
 * access**. A batch app (ADR §107) serves no pages and gets nothing.
 *
 * Shared by the apps list and the tag pages, which drew the same objects on
 * two sets of rules: one with this button, one with a mono URL column and a
 * hover-only copy that a touch screen never shows.
 *
 * A link, not a button with an onClick: middle-click, ⌘-click and "copy link
 * address" all work. stopPropagation keeps it from also triggering the row's
 * navigation to the detail page.
 */
export function AppOpenAction({
  app,
}: {
  app: Pick<AppWithMetrics, "id" | "name" | "slug" | "framework" | "can_open" | "status">;
}) {
  if (isBatchFramework(app.framework)) return null;
  if (app.can_open) {
    return (
      <a
        href={appUrl(app)}
        target="_blank"
        rel="noopener noreferrer"
        onClick={(e) => e.stopPropagation()}
        {...wakeAheadProps(app)}
        aria-label={`Open ${app.name}`}
        className={buttonVariants({ variant: "outline", size: "xs" })}
      >
        Open
        <ExternalLink data-icon="inline-end" />
      </a>
    );
  }
  return (
    <Link
      to={`/apps/${app.id}`}
      onClick={(e) => e.stopPropagation()}
      aria-label={`Request access to ${app.name}`}
      className={buttonVariants({ variant: "outline", size: "xs" })}
    >
      Request access
    </Link>
  );
}
