import type { ComponentProps } from "react";

import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

/**
 * An icon-only button whose name is also shown on hover and focus.
 *
 * Every icon-only control here carried an `aria-label`, and none said anything
 * to a sighted reader wondering what the copy glyph or the `⋯` would do. The
 * label is required and is the tooltip, so the two cannot disagree. Works as a
 * menu trigger: `<DropdownMenuTrigger render={<IconButton label=… />} />`
 * passes its props and ref straight through to the button.
 */
export function IconButton({
  label,
  size = "icon-sm",
  variant = "ghost",
  ...props
}: Omit<ComponentProps<typeof Button>, "aria-label"> & { label: string }) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={<Button variant={variant} size={size} aria-label={label} {...props} />}
      />
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  );
}
