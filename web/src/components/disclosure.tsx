import { useState, type ReactNode } from "react";
import { ChevronDown } from "lucide-react";

import { cn } from "@/lib/utils";

/**
 * One shape for "there is more here if you want it".
 *
 * The product had three. `SettingRow`'s *Why this matters* drew a rotating
 * chevron; the app's Preview drew a rotating chevron of its own; and five other
 * places used a bare `<details>`, which means the browser's `▶` triangle — a
 * glyph that appears nowhere else in this interface and does not rotate the way
 * the other two do. Somebody learning that a chevron means *expand* had to learn
 * it twice.
 *
 * A `<button>` rather than `<details>`/`<summary>`, and that is the load-bearing
 * part rather than a styling preference. A closed `<details>` still has its
 * children in the DOM — so a hidden `<iframe>` still fetches, a hidden query
 * still mounts, and "it only loads when opened" is a thing the element does not
 * do. The app's own Preview had to track `open` in state for exactly that
 * reason. Rendering the children on `open` makes that the default rather than
 * something each caller has to remember.
 *
 * `<details>` is still right where the content is inert and printable — the
 * error boundary's stack trace is one — so this does not claim to replace it
 * everywhere. It replaces it where the triangle was showing.
 */
export function Disclosure({
  label,
  children,
  className,
  size = "sm",
  defaultOpen = false,
  onOpenChange,
}: {
  /** What is behind it, named as a thing rather than as an instruction. */
  label: ReactNode;
  children: ReactNode;
  className?: string;
  /** `xs` inside a dense row, `sm` standing on a page. */
  size?: "xs" | "sm";
  defaultOpen?: boolean;
  /** For a caller that has to know — one that only mounts something on open. */
  onOpenChange?: (open: boolean) => void;
}) {
  const [open, setOpen] = useState(defaultOpen);

  function toggle() {
    const next = !open;
    setOpen(next);
    onOpenChange?.(next);
  }

  return (
    <div className={className}>
      <button
        type="button"
        onClick={toggle}
        aria-expanded={open}
        className={cn(
          "flex items-center gap-1 rounded-sm font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none",
          size === "xs" ? "text-xs" : "text-sm",
        )}
      >
        <ChevronDown
          className={cn("h-3.5 w-3.5 transition-transform", !open && "-rotate-90")}
          aria-hidden
        />
        {label}
      </button>
      {open && <div className="mt-2">{children}</div>}
    </div>
  );
}
