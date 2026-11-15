import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/**
 * Whether this system role may publish — the install-wide half of the server's
 * `ActionPublish` (`internal/auth/authorize.go`). For deciding whether to *ask*
 * for something only publishers may read, never for hiding what the server
 * would refuse anyway.
 */
export function mayPublish(role?: string): boolean {
  return role === "publisher" || role === "system_admin"
}

/**
 * How a system role is named to a person. The stored values are
 * `system_admin` / `publisher` / `viewer`, and the first of those is a schema
 * detail nobody should have to read — render this, never the raw value.
 */
export function systemRoleLabel(role?: string): string {
  switch (role) {
    case "system_admin":
      return "Admin"
    case "publisher":
      return "Publisher"
    case "viewer":
      return "Viewer"
    // The rung below every way of reading: the app is in their list and they
    // cannot open it. "List only" rather than "List", because a one-word label
    // beside Viewer and Editor reads as a weaker kind of access rather than as
    // none of it.
    case "list":
      return "List only"
    // Not a role, and the one place it is displayed as if it were: a provider's
    // "role for new accounts" control, where it is the answer "none yet".
    case "pending":
      return "Pending activation"
    default:
      return role ?? ""
  }
}

/** The role as the end of "alice is now …" — lowercase, with its article,
 * because "a admin" is what `a ${label}` made of the one role that starts
 * with a vowel. */
export function systemRoleInSentence(role: "system_admin" | "publisher" | "viewer"): string {
  const label = systemRoleLabel(role).toLowerCase()
  return `${/^[aeiou]/.test(label) ? "an" : "a"} ${label}`
}

/** Same idea for a grant's role on one app: `owner` / `editor` /
 * `viewer` are stored lowercase and read as titles. */
export function grantRoleLabel(role?: string): string {
  switch (role) {
    case "owner":
      return "Owner"
    case "editor":
      return "Editor"
    case "viewer":
      return "Viewer"
    // The same rung as `systemRoleLabel`'s, and it was missing here — so a
    // grant *at* that rung fell through to `default` and rendered as the raw
    // word `list` on the Access table, in the role picker and in the toast
    // that reports a change. Same wording for the same reason: one word beside
    // Viewer and Editor reads as a weaker kind of access rather than as none
    // of it.
    case "list":
      return "List only"
    // Not a role, and the one place it is displayed as if it were: a provider's
    // "role for new accounts" control, where it is the answer "none yet".
    case "pending":
      return "Pending activation"
    default:
      return role ?? ""
  }
}

/**
 * A server-supplied fragment, rendered as a sentence.
 *
 * Some messages arrive lower case and without a stop, because they were written
 * to be appended to something — and then a page put one on a line of its own,
 * where it reads as a sentence that lost its first word. Pages should not each
 * decide how to patch that up.
 */
export function sentence(text: string): string {
  const trimmed = text.trim();
  if (!trimmed) return trimmed;
  const opened = trimmed[0].toUpperCase() + trimmed.slice(1);
  return /[.!?…]$/.test(opened) ? opened : `${opened}.`;
}

/**
 * The id a `FieldError` carries, for its control's `aria-describedby`.
 *
 * Here rather than beside the component because a `.tsx` exporting both a
 * component and a plain function cannot hot-reload as a unit — the same reason
 * `buttonVariants` has a module of its own. What it buys is that the string is
 * written once: an `aria-describedby` naming an id no element has is a refusal
 * a screen reader never reaches, and nothing reports it.
 */
export function fieldErrorId(field: string): string {
  return `${field}-error`;
}
