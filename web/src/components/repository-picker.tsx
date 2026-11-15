import { toneText } from "@/lib/tone";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Check, Lock, Search, X } from "lucide-react";

import { api } from "@/lib/api";
import type { PickerRepository } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/empty-state";
import { FieldHelp } from "@/components/field-help";
import { QueryError } from "@/components/query-error";
import { Notice } from "@/components/notice";
import { TableCard } from "@/components/table-card";
import { toneSurface } from "@/lib/tone";

/**
 * Pick a repository from a connected git host.
 *
 * **The list is what this person can see**, fetched through their own
 * credential against the host and never through the install's — an install-wide
 * credential sees everything the organization exposed, so listing with it would
 * hand every signed-in developer a directory of the company's private
 * repositories.
 *
 * **It filters and it refuses nothing.** A repository the source policy would
 * turn away is shown disabled with the reason rather than hidden: a developer
 * who can see why does not open a ticket, and the URL field beside this can
 * still be typed into — which is exactly why the picker is not an enforcement
 * point.
 *
 * No cache, so the search debounces to a submit rather than firing per
 * keystroke: a cache of what a person may see is a cache of an authorization
 * answer, and the cost of not having one is the host's rate limits in the
 * request path.
 */
export function RepositoryPicker({
  connectionId,
  connectionName,
  onPick,
}: {
  connectionId: string;
  connectionName: string;
  onPick: (repo: PickerRepository) => void;
}) {
  const [query, setQuery] = useState("");
  const [submitted, setSubmitted] = useState("");
  // Which row was picked, so the list says so: the URL field it fills is
  // further down, often below the list's own scroll.
  const [picked, setPicked] = useState<string | null>(null);

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["connection-repos", connectionId, submitted],
    queryFn: () => api.listConnectionRepos(connectionId, submitted),
  });

  if (error) {
    return (
      <QueryError
        error={error}
        title={`${connectionName} could not be reached`}
        fallback="Enter the repository URL below instead, or try again."
        onRetry={() => refetch()}
      />
    );
  }

  if (data?.needs_authorize) {
    return (
      <Notice tone="info" size="sm" title={`Sign in to ${connectionName}`}>
        <p>
          Sign in to {connectionName} once to list the repositories you can see. You can still type
          a URL below without signing in.
        </p>
        <p className="mt-2">
          <Button size="sm" onClick={() => { window.location.href = data.authorize_url!; }}>
            Sign in to {connectionName}
          </Button>
        </p>
      </Notice>
    );
  }

  const repositories = data?.repositories ?? [];

  function clear() {
    setQuery("");
    setSubmitted("");
  }

  return (
    <div className="space-y-2">
      <Label htmlFor="repo-search">Repository</Label>
      <form
        className="flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setSubmitted(query.trim());
        }}
      >
        <Input
          id="repo-search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Escape") clear(); }}
          placeholder={`Search ${connectionName}…`}
        />
        {/* Not `SearchInput`, and the reason is the button beside it: this
            search is *submitted*, against somebody else's API, so a debounce
            would be a request per quarter-second of typing at GitHub. What it
            borrows is the half that was missing — Escape and a way back to the
            whole list, which the empty state below was already telling people
            to use. */}
        {submitted && (
          <Button type="button" variant="ghost" size="icon" aria-label="Clear the search" onClick={clear}>
            <X className="size-4" aria-hidden="true" />
          </Button>
        )}
        <Button type="submit" variant="outline" size="icon" aria-label="Search repositories">
          <Search className="size-4" aria-hidden="true" />
        </Button>
      </form>

      {isLoading ? (
        <Skeleton className="h-40 w-full" />
      ) : repositories.length === 0 ? (
        <TableCard className="p-0">
          <EmptyState
            title="Nothing to show"
            description={
              submitted
                ? `Nothing on ${connectionName} matches “${submitted}”. Clear the search, or enter the repository URL below.`
                : `No repositories to show from ${connectionName}. Type the repository URL below instead.`
            }
            action={
              // An empty state that names an act offers it. It said "Clear the
              // search" beside a field that had no clear.
              submitted ? (
                <Button variant="secondary" size="sm" onClick={clear}>
                  Clear the search
                </Button>
              ) : undefined
            }
          />
        </TableCard>
      ) : (
        <TableCard className="max-h-72 overflow-y-auto p-0">
          <ul className="divide-y divide-border/60">
            {repositories.map((repo) => (
              <li key={repo.full_name}>
                <button
                  type="button"
                  disabled={!repo.allowed || !repo.covered}
                  aria-pressed={picked === repo.full_name}
                  onClick={() => {
                    setPicked(repo.full_name);
                    onPick(repo);
                  }}
                  className="flex w-full flex-col items-start gap-1 px-3 py-2 text-left transition-colors hover:bg-muted/50 disabled:cursor-not-allowed disabled:opacity-70 disabled:hover:bg-transparent"
                >
                  <span className="flex flex-wrap items-center gap-2">
                    {picked === repo.full_name && (
                      <Check className="size-3.5 text-primary" aria-hidden="true" />
                    )}
                    <span className="font-mono text-sm">{repo.full_name}</span>
                    {repo.private && (
                      <Lock className="size-3 text-muted-foreground" aria-label="Private" />
                    )}
                    {repo.archived && (
                      <Badge variant="outline" className={`text-2xs ${toneSurface.neutral}`}>
                        Archived
                      </Badge>
                    )}
                  </span>
                  {repo.description && (
                    <span className="line-clamp-1 text-2xs text-muted-foreground">
                      {repo.description}
                    </span>
                  )}
                  {/* The reason, in text and never in a title: a disabled
                      element is not focusable and swallows mouse events, so a
                      tooltip on it is unreachable by every route. */}
                  {!repo.allowed && repo.refusal && (
                    <span className={`text-2xs ${toneText.danger}`}>{repo.refusal}</span>
                  )}
                  {repo.allowed && !repo.covered && (
                    <span className={`text-2xs ${toneText.danger}`}>
                      {connectionName} does not cover this repository.
                    </span>
                  )}
                </button>
              </li>
            ))}
          </ul>
        </TableCard>
      )}
      {data?.has_more && repositories.length > 0 && (
        <FieldHelp>
          {submitted
            ? "Showing the first 100 matches. Narrow the search to find others."
            : "Showing the first 100. Search to find others."}
        </FieldHelp>
      )}
    </div>
  );
}
