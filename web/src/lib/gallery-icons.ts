import { ArrowRightLeft, type LucideIcon } from "lucide-react";

/**
 * The glyphs a gallery item may name in the catalog's `icon` field. A name,
 * never a URL: the catalog is fetched from outside, and a remote image would be
 * a request to a third party from every viewer's browser (`internal/gallery`'s
 * `Item.Icon`). A name this build does not know falls back to the language's
 * mark, so a newer catalog never breaks an older install.
 *
 * No prototype, for the same reason: on a plain literal `"constructor"` or
 * `"__proto__"` found `Object`'s own members, and the Gallery crashed for every
 * reader instead of falling back.
 */
export const GALLERY_ICONS: Readonly<Record<string, LucideIcon | undefined>> = Object.assign(
  Object.create(null) as Record<string, LucideIcon>,
  {
    "arrow-right-left": ArrowRightLeft,
  },
);
