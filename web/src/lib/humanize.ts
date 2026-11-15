/** Words a reader knows as letters: `repo_url` is *Repo URL*, `id` is *ID*. */
const ACRONYMS: Record<string, string> = {
  id: "ID", url: "URL", ip: "IP", api: "API", oidc: "OIDC", scim: "SCIM", cpu: "CPU",
  sha: "SHA", tls: "TLS", uuid: "UUID", json: "JSON",
};

/** `current_role_since` → "Current role since"; `repo_url` → "Repo URL". */
export function humanize(key: string): string {
  const words = key
    .split("_")
    .filter(Boolean)
    .map((w) => ACRONYMS[w.toLowerCase()] ?? w.toLowerCase());
  const out = words.join(" ");
  return out.charAt(0).toUpperCase() + out.slice(1);
}
