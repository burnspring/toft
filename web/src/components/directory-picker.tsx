import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { UserRound, Users } from "lucide-react";

import { api } from "@/lib/api";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { toneText } from "@/lib/tone";

/**
 * Find a person or a group and pick one.
 *
 * Six surfaces had hand-rolled the same thing — a search box, a two-character
 * gate before the query fires, a `max-h-48` scrolling list of candidates, and
 * an empty state — and they had drifted: different heights, different empty
 * copy, and none of them debounced, while `SearchInput`'s 250ms debounce was
 * being spent on two purely local filters.
 *
 * `exclude` is a predicate rather than a set of ids, because "already on the
 * list" is not the same question everywhere: on a store's People section
 * somebody with a grant on the whole store may still be given a narrower one
 * on a folder, so the answer depends on which folder is being granted into.
 * `rowSuffix` is the muted text at the end of each row,
 * which is where each caller says what picking will *do* — "add with write",
 * "make an editor" — because the same picker adds at different levels in
 * different places and a row that does not say so is a click into the unknown.
 */
export function DirectoryPicker({
  id,
  label,
  placeholder = "Search by name…",
  minChars = 2,
  exclude,
  onPick,
  rowSuffix,
  pendingKey,
}: {
  id: string;
  label: string;
  placeholder?: string;
  minChars?: number;
  /** True for a candidate that is already on the caller's list. */
  exclude?: (kind: "user" | "group", id: string) => boolean;
  onPick: (kind: "user" | "group", id: string, label: string) => void;
  rowSuffix?: (kind: "user" | "group", id: string) => ReactNode;
  /** The `kind:id` currently being added, so its row can go quiet. */
  pendingKey?: string;
}) {
  const [search, setSearch] = useState("");
  const q = search.trim();
  const open = q.length >= minChars;

  const usersQuery = useQuery({
    queryKey: ["directory-users", q],
    queryFn: () => api.searchDirectoryUsers(q),
    enabled: open,
  });
  const groupsQuery = useQuery({
    queryKey: ["directory-groups", q],
    queryFn: () => api.searchDirectoryGroups(q),
    enabled: open,
  });
  const users = usersQuery.data ?? [];
  const groups = groupsQuery.data ?? [];
  // Three states, and the two that were missing are the ones that look like
  // the third. `data = []` while a search is in flight said "No matching
  // people or groups." for the whole round trip of every keystroke, and said
  // exactly the same thing when the directory read *failed* — so a broken
  // search and an empty directory were the same sentence, and the sentence
  // was a claim about the directory rather than about the request.
  const searching = usersQuery.isFetching || groupsQuery.isFetching;
  const failed = usersQuery.error ?? groupsQuery.error;

  const skip = exclude ?? (() => false);
  const userCandidates = users.filter((u) => !skip("user", u.id));
  const groupCandidates = groups.filter((g) => !skip("group", g.id));

  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        placeholder={placeholder}
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        autoComplete="off"
      />
      {/* How many matched, said out loud. A results list that appears under an
          input announces nothing on its own: a reader who cannot see it types
          a name and is told neither that anything arrived nor how much. */}
      <p aria-live="polite" className="sr-only">
        {!open
          ? ""
          : failed
            ? "The directory could not be searched."
            : searching
              ? "Searching…"
              : `${userCandidates.length + groupCandidates.length} matches`}
      </p>
      {open && (
        <div className="max-h-48 divide-y divide-border/60 overflow-y-auto rounded-md border border-border/60">
          {failed ? (
            <p className={`p-2.5 text-xs ${toneText.danger}`}>
              {failed instanceof Error && failed.message
                ? failed.message
                : "The directory could not be searched."}
            </p>
          ) : searching ? (
            <p className="p-2.5 text-xs text-muted-foreground">Searching…</p>
          ) : userCandidates.length === 0 && groupCandidates.length === 0 ? (
            <p className="p-2.5 text-xs text-muted-foreground">No matching people or groups.</p>
          ) : (
            <>
              {userCandidates.map((u) => (
                <button
                  key={u.id}
                  type="button"
                  disabled={pendingKey === `user:${u.id}`}
                  className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:opacity-50"
                  onClick={() => {
                    onPick("user", u.id, u.login);
                    setSearch("");
                  }}
                >
                  {/* A person and a group get different icons, as AppOwner
                      draws them: a group is not a person with an odd name. */}
                  <UserRound className="h-4 w-4 text-muted-foreground" aria-hidden />
                  <span className="font-medium">{u.login}</span>
                  {rowSuffix && (
                    <span className="ml-auto text-xs text-muted-foreground">
                      {rowSuffix("user", u.id)}
                    </span>
                  )}
                </button>
              ))}
              {groupCandidates.map((g) => (
                <button
                  key={g.id}
                  type="button"
                  disabled={pendingKey === `group:${g.id}`}
                  className="flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm transition-colors hover:bg-muted/50 disabled:opacity-50"
                  onClick={() => {
                    onPick("group", g.id, g.name);
                    setSearch("");
                  }}
                >
                  <Users className="h-4 w-4 text-muted-foreground" aria-hidden />
                  <span className="font-medium">{g.name}</span>
                  <span className="ml-auto text-xs text-muted-foreground">
                    {rowSuffix ? <>group · {rowSuffix("group", g.id)}</> : "group"}
                  </span>
                </button>
              ))}
            </>
          )}
        </div>
      )}
    </div>
  );
}
