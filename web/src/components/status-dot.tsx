import { cn } from "@/lib/utils";
import { statusIsLive, statusMeaning } from "@/lib/status";
import { toneSolid } from "@/lib/tone";

/**
 * A state, as a dot — the compact form of `StatusBadge`, for a row that has
 * already spent its width. Same vocabulary, same colours, from `lib/status.ts`.
 */
export function StatusDot({
  status,
  showLabel = false,
  className,
}: {
  status: string;
  showLabel?: boolean;
  className?: string;
}) {
  const { label, tone } = statusMeaning(status);
  const color = toneSolid[tone];

  return (
    <span className={cn("inline-flex items-center gap-1.5", className)}>
      {/* A bare dot is a colour, and a colour is not a status to anyone who
          cannot see it — so the dot carries the label itself when nothing else
          is drawing it, and steps out of the way when the text is right there. */}
      <span
        className="relative flex h-2 w-2"
        {...(showLabel ? { "aria-hidden": true } : { role: "img", "aria-label": label })}
      >
        {statusIsLive(status) && (
          <span
            className={cn(
              "absolute inline-flex h-full w-full animate-ping rounded-full opacity-75",
              color,
            )}
          />
        )}
        <span className={cn("relative inline-flex h-2 w-2 rounded-full transition-colors duration-300", color)} />
      </span>
      {showLabel && (
        <span className="text-xs font-medium text-muted-foreground">{label}</span>
      )}
    </span>
  );
}
