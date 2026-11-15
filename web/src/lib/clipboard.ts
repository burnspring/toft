import { toast } from "sonner";

/**
 * Copy something, and say so the same way every time.
 *
 * Nine handlers did this and they had five success phrasings between them —
 * "App ID copied to clipboard", "Name copied", "Copied to the clipboard" — and
 * four failure ones. Four of them also reported success without awaiting the
 * write, so a refused clipboard (an insecure origin, a permission the browser
 * declined) said "copied" and copied nothing, which is the one outcome a person
 * finds out about at the point they paste.
 *
 * `subject` is a lower-case noun phrase — "app ID", "URL", "name". The success
 * toast sentence-cases it and the failure sentence uses it as it is, which is
 * why an initialism like "URL" comes through unharmed.
 *
 * It resolves to whether the write landed, for the caller whose next state
 * depends on it — `OnceOnlySecret` must not say "copied" over a refusal.
 */
export async function copyToClipboard(text: string, subject: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    toast.success(`${subject.charAt(0).toUpperCase()}${subject.slice(1)} copied`);
    return true;
  } catch {
    toast.error(`Couldn’t copy the ${subject} — select it and copy it by hand`);
    return false;
  }
}
