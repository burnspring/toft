import type { ReactNode } from "react";

import { Notice } from "@/components/notice";

/**
 * What a write-only credential field says when the value behind it cannot be
 * decrypted.
 *
 * Every settings form here treats an empty field as "keep the stored value",
 * which is the right idiom for a secret that never comes back out — and it
 * becomes a trap the moment `ENCRYPTION_KEY` changes or is lost. The column is
 * still non-empty, so the form still says "set" and still offers to keep it,
 * and an administrator who saves without retyping believes they preserved
 * something and has preserved nothing.
 *
 * The key id on each stored record is what lets the server tell "set and
 * readable" from "set and unreadable" at all (ADR §62). This is the sentence
 * that difference buys, and it is one component rather than five copies so
 * that five pages cannot describe the same condition five ways.
 *
 * `danger` rather than `attention`: this is not a warning about something that
 * might matter later. The stored credential is unusable now, and whatever it
 * authenticated has already stopped working.
 *
 * `replace` is for the one caller that does not render this beside the field:
 * on a table row there is no "this field" to leave blank, and the sentence has
 * to name the door instead.
 */
export function UnreadableCredentialNotice({
  what,
  replace,
}: {
  what: string;
  replace?: ReactNode;
}) {
  return (
    <Notice tone="danger" title="Stored, but unreadable">
      This install&rsquo;s encryption key cannot decrypt the stored {what}, so it
      no longer authenticates anything.{" "}
      {replace ?? (
        <>
          Leaving this field blank keeps the unreadable value — type the {what}{" "}
          again to replace it.
        </>
      )}{" "}
      If the key was
      changed by mistake, restoring it (as <code>ENCRYPTION_KEY</code> or in{" "}
      <code>ENCRYPTION_KEY_OLD</code>) recovers this and everything else stored
      under it.
    </Notice>
  );
}
