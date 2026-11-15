import type { ReactNode } from "react";
import { useDocumentTitle } from "@/hooks/use-document-title";

interface PageHeaderProps {
  title: string;
  /** A sentence; it may carry a link to the page that answers the next question. */
  description?: ReactNode;
  badge?: ReactNode;
  actions?: ReactNode;
  /**
   * What the browser tab says, when it should differ from the heading —
   * "Apps" reads better on a tab than the h1's own wording would in a few
   * places. Rarely needed.
   */
  documentTitle?: string;
}

export function PageHeader({
  title,
  description,
  badge,
  actions,
  documentTitle,
}: PageHeaderProps) {
  // The page already declares its name here; there is no reason for a second
  // declaration somewhere else to say what the tab is called.
  useDocumentTitle(documentTitle ?? title);

  return (
    <div className="mb-8 flex flex-wrap items-start justify-between gap-4">
      <div>
        <div className="flex items-center gap-3">
          <h1 className="text-2xl font-semibold tracking-tight text-foreground">{title}</h1>
          {badge}
        </div>
        {/* The measure, and it is the product's own: `max-w-prose` is 65ch,
            which `setting-row.tsx` has set on all three of a setting's prose
            slots since it was written. Unbounded, a page description ran to
            141 characters on a 1440px viewport. A ceiling only ever removes
            width, so a description that is one short line loses nothing. */}
        {description && (
          <p className="mt-1 max-w-prose text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {/* `shrink-0` keeps the actions whole and the heading gives way; the
          wrap on the row above is what happens when the heading has nowhere
          left to give. */}
      {actions && <div className="flex flex-wrap items-center gap-2 shrink-0">{actions}</div>}
    </div>
  );
}
