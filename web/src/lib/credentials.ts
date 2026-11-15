/**
 * The placeholder a write-only credential field shows.
 *
 * Three states, and the third is the one the key id on each stored record made
 * expressible (ADR §62): nothing stored, something stored and readable, and
 * something stored that this install's key cannot decrypt. The last one must
 * never read "(unchanged)" — that is the sentence that tells an administrator
 * their unusable value is safely kept.
 *
 * Lives here rather than beside UnreadableCredentialNotice because a module
 * that exports both a component and a function breaks fast refresh.
 */
export function credentialPlaceholder(
  set: boolean,
  unreadable: boolean,
  empty: string,
): string {
  if (unreadable) return "•••••••• (unreadable — retype)";
  if (set) return "•••••••• (unchanged)";
  return empty;
}
