/**
 * The redirect URI a provider not yet created will have.
 *
 * The server derives it from the install's BASE_URL and the provider id, and
 * says so on every provider it lists. The browser's own origin is a different
 * address whenever the install is reached through a tunnel or an internal
 * hostname, and an administrator who registered that one at the IdP met a
 * redirect mismatch on the first sign-in. So the base is read back off a
 * provider the server already described, and the browser's origin is the
 * fallback only where none has one.
 */
export function redirectURIPreview(
  listed: { id: string; redirect_uri: string }[],
  id: string,
  origin: string,
): string {
  let base = origin;
  for (const p of listed) {
    const suffix = `/api/v1/auth/${p.id}/callback`;
    if (p.redirect_uri.endsWith(suffix)) {
      base = p.redirect_uri.slice(0, -suffix.length);
      break;
    }
  }
  return `${base}/api/v1/auth/${id || "<id>"}/callback`;
}
