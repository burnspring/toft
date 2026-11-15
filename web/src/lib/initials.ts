/**
 * Two letters that tell people apart. Taking the first two characters of a
 * login made `admin`, `admin-one` and `admin-two` all "AD", which is three
 * avatars carrying no information. A name's words come first, then a login's
 * parts (`admin-one` → "AO"), and only a single word falls back to its first
 * two letters.
 */
export function initials(name: string | undefined, login: string | undefined): string {
  const words = (s: string | undefined) => (s ?? "").split(/[\s._@-]+/).filter(Boolean);
  for (const w of [words(name), words(login)]) {
    if (w.length >= 2) return (w[0]![0]! + w[1]![0]!).toUpperCase();
  }
  const one = words(name)[0] ?? words(login)[0] ?? "";
  return one.slice(0, 2).toUpperCase() || "?";
}
