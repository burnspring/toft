/**
 * Whether a URL from outside data may be an `href`: http or https, nothing
 * else. An advisory's reference URL comes verbatim from a downloaded feed, and
 * React renders a `javascript:` href with a warning rather than refusing it —
 * so a poisoned record would run in an administrator's session on the click.
 */
export function isHttpUrl(value: string | undefined | null): value is string {
  if (!value) return false;
  try {
    const { protocol } = new URL(value);
    return protocol === "http:" || protocol === "https:";
  } catch {
    return false;
  }
}
