/**
 * Where rsconnect sends a publisher after a redeploy (ADR §109).
 *
 * It builds `{server}/connect/#/apps/{guid}/{access|logs}` itself, in Posit
 * Connect's own shape, and the part that names the app is a fragment the
 * server never sees — so the redirect can only happen here. The guid is the
 * app id and both tabs exist, so the address maps across unchanged; anything
 * else lands on the app list rather than on a 404.
 */
export function connectRedirectTarget(hash: string): string {
  const m = /^#\/apps\/([0-9a-f-]{36})(?:\/(access|logs))?\/?$/i.exec(hash);
  if (!m) return "/";
  return m[2] ? `/apps/${m[1]}/${m[2]}` : `/apps/${m[1]}`;
}
