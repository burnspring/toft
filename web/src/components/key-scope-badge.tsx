import { Badge } from "@/components/ui/badge";
import { toneSurface } from "@/lib/tone";
import type { UserKeyScope } from "@/lib/api";

/** What a personal key may call, as a word.
 *
 * A scope is not a state, so this is not a `StatusBadge`: nothing is happening
 * to a key because it is read-scoped. `neutral` is a recorded fact with no
 * valence, which is exactly what a bound somebody chose at creation is — and
 * `info` reads as the platform acting, which this is not.
 *
 * Its own component rather than the one in `pages/admin/deploy-keys.tsx`,
 * because the two classes share the word `read` and nothing else: a deploy
 * key's other scope is `deploy` and belongs to nobody, and a badge that took
 * both vocabularies would have to be told which class it was rendering on every
 * call. Same argument, same tone, two small components.
 */
const SCOPE_LABEL: Record<UserKeyScope, string> = {
  read: "Read",
  full: "Full",
};

export function KeyScopeBadge({ scope }: { scope: UserKeyScope }) {
  return (
    <Badge variant="outline" className={toneSurface.neutral}>
      {SCOPE_LABEL[scope] ?? SCOPE_LABEL.full}
    </Badge>
  );
}
