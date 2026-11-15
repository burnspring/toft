import type { ReactNode } from "react";
import { AlertTriangle, CheckCircle2, Info, OctagonAlert } from "lucide-react";
import { cn } from "@/lib/utils";
import { toneSurface, type Tone } from "@/lib/tone";

/**
 * A tinted block of explanatory copy: the thing this product says when a
 * capability is off, a source is no longer allowed, an app has crashed, or a
 * setting somebody just changed has a consequence they should read.
 *
 * There were sixty of these written by hand, no two quite alike — `p-3
 * text-xs`, `px-4 py-3 text-sm`, `amber-800`, `amber-900`, `amber-50/60`,
 * `border-amber-200`, `border-amber-300` — which is what happens when the most
 * important copy in a product has no component. They are the sentences a person
 * reads at the exact moment something has gone wrong, and they were the least
 * consistent surface in the codebase.
 *
 * **The icon is not decoration and is not optional by default.** A tinted
 * background is colour carrying meaning on its own, which is the oldest
 * accessibility defect there is; the icon is what makes an amber block and a
 * red block distinguishable to somebody who cannot tell them apart. Pass
 * `icon={null}` only where a neighbouring element already says which kind this
 * is.
 *
 * `live` is off by default and should stay off for anything rendered from
 * page state. Turn it on where the notice *appears* in response to something
 * the person just did — otherwise every navigation re-announces a condition
 * they already know about.
 */

const TONE_ICON: Record<Tone, typeof Info> = {
  ok: CheckCircle2,
  attention: AlertTriangle,
  danger: OctagonAlert,
  neutral: Info,
  info: Info,
};

interface NoticeProps {
  /** Defaults to `attention` — by a long way the most common of these. */
  tone?: Tone;
  /** A short lead. Omit for a one-sentence notice; the body carries it. */
  title?: ReactNode;
  children?: ReactNode;
  /** The way out: a link to the page that fixes this, or a button. */
  action?: ReactNode;
  /** `null` suppresses it. Anything else replaces the tone's default. */
  icon?: ReactNode | null;
  /** `sm` sits inside a section (the default); `md` leads a page. */
  size?: "sm" | "md";
  /** Announce it when it appears. See the note above before turning this on. */
  live?: boolean;
  className?: string;
}

export function Notice({
  tone = "attention",
  title,
  children,
  action,
  icon,
  size = "sm",
  live = false,
  className,
}: NoticeProps) {
  const Icon = TONE_ICON[tone];
  const glyph =
    icon === null ? null : (icon ?? <Icon className="h-4 w-4 shrink-0" aria-hidden />);

  return (
    <div
      role={live ? "status" : undefined}
      aria-live={live ? "polite" : undefined}
      className={cn(
        "flex gap-2.5 rounded-lg border",
        size === "sm" ? "px-3 py-2.5 text-xs" : "px-4 py-3 text-sm",
        toneSurface[tone],
        className,
      )}
    >
      {glyph && <span className={cn("mt-px", size === "md" && "mt-0.5")}>{glyph}</span>}
      <div className="flex min-w-0 flex-1 flex-wrap items-start justify-between gap-x-4 gap-y-2">
        {/* The measure, on each prose slot rather than on the column that
            holds them. Bounding the column would be the same width today —
            these two are its only children — and it would be the wrong place
            to have written it the first time a notice grows something that is
            not prose beside them. The two that already hold structure, the
            refused-deploy banner's `<pre>` and Settings' deferring list, are
            both inside the body and narrow correctly.

            This is the component that needed it most: the worst line in the
            product was a notice body at 197 characters, 12px, indigo on
            indigo, on the page an administrator opens to decide who can get
            in. A line that long is not unreadable, it is skipped — and these
            are the sentences somebody reads at the moment something has gone
            wrong. */}
        <div className="min-w-0 flex-1 space-y-1">
          {title && <p className="max-w-prose font-medium">{title}</p>}
          {children && (
            // The body in the reading colour; the tone is carried by the
            // surface, the border, the icon and the title. A paragraph set
            // entirely in amber or indigo is tiring to read and makes a long
            // notice look louder than a short one.
            <div className="max-w-prose text-foreground [&_a]:underline [&_a]:underline-offset-2">{children}</div>
          )}
        </div>
        {action && <div className="flex shrink-0 items-center gap-2">{action}</div>}
      </div>
    </div>
  );
}
