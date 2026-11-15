// Tag display helpers. Kept out of the chip component so they can be imported
// from tables, filters, and pages without dragging a component in.

import type { AppTag, TagColor } from "./api";

/**
 * Chip palette. Tags carry a palette key rather than free hex so every chip
 * stays inside the design system and admin input is bounded.
 */
const palette: Record<TagColor, string> = {
  indigo: "bg-indigo-50 text-indigo-700 border-indigo-200",
  cyan: "bg-cyan-50 text-cyan-700 border-cyan-200",
  fuchsia: "bg-fuchsia-50 text-fuchsia-700 border-fuchsia-200",
  green: "bg-green-50 text-green-700 border-green-200",
  amber: "bg-amber-50 text-amber-800 border-amber-200",
  red: "bg-red-50 text-red-700 border-red-200",
  violet: "bg-violet-50 text-violet-700 border-violet-200",
  orange: "bg-orange-50 text-orange-700 border-orange-200",
  slate: "bg-slate-100 text-slate-700 border-slate-200",
};

export const TAG_COLORS = Object.keys(palette) as TagColor[];

export function tagColorClass(color?: TagColor) {
  return palette[color ?? "slate"] ?? palette.slate;
}

/**
 * A tag's colour as a solid dot. The chip's 50/100 tint is a background meant
 * to carry text; drawn as a 10px dot on white, slate's nearly vanished.
 */
const dotPalette: Record<TagColor, string> = {
  indigo: "bg-indigo-500",
  cyan: "bg-cyan-500",
  fuchsia: "bg-fuchsia-500",
  green: "bg-green-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  violet: "bg-violet-500",
  orange: "bg-orange-500",
  slate: "bg-slate-500",
};

export function tagDotClass(color?: TagColor) {
  return dotPalette[color ?? "slate"] ?? dotPalette.slate;
}

/** The browse URL for one key/value pair. */
export function tagHref(tag: Pick<AppTag, "tag_slug" | "value_slug">) {
  return `/tags/${tag.tag_slug}/${tag.value_slug}`;
}
