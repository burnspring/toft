import { Box, Layers } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { executionModeLabel } from "@/lib/execution-mode";
import type { ExecutionMode } from "@/lib/api";
import { cn } from "@/lib/utils";

/**
 * Where an app runs, as a chip.
 *
 * Neutral and outlined like `FrameworkBadge`, and for the same reason: a mode
 * is a classification rather than a state. Giving it a tone would put a
 * running isolated app in the same colour vocabulary as a crashed one, and
 * there is nothing to be alarmed or reassured about in either mode — an
 * install chose it. The icon, not a colour, is what tells the two apart.
 */
export function ModeBadge({ mode, className }: { mode: ExecutionMode; className?: string }) {
  const Icon = mode === "isolated" ? Box : Layers;
  return (
    <Badge
      variant="outline"
      className={cn("gap-1.5 font-normal text-2xs text-muted-foreground", className)}
    >
      <Icon className="opacity-60" aria-hidden />
      {executionModeLabel[mode]}
    </Badge>
  );
}

/**
 * The sentence that goes under the badge when the app is set to move.
 *
 * `attention` rather than muted grey: nothing is wrong, but the mode on the
 * screen is not the mode the app will have after its next deploy, and reading
 * one for the other is exactly the mistake worth spending a colour on.
 */
export function ModePending({ mode }: { mode?: ExecutionMode }) {
  if (!mode) return null;
  return (
    <span className="mt-1 block text-2xs text-tone-attention-fg">
      {executionModeLabel[mode].toLowerCase()} on next deploy
    </span>
  );
}
