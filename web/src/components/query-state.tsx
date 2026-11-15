import type { ReactNode } from "react";

import { QueryError } from "@/components/query-error";

/** The parts of a `useQuery` result this needs. Structural, so it takes any of
 *  them without importing react-query's union of result types. */
export interface QueryLike {
  isLoading: boolean;
  error: unknown;
  refetch?: () => unknown;
}

/**
 * The three states, in the one order that is right, made the short way.
 *
 * Loading, then **broken**, then empty, then the content. The middle one is the
 * one that goes missing: a component that renders `EmptyState` from
 * `data ?? []` says "No API keys yet. Create one to get started." when the
 * fetch failed — an invitation to do something, at the moment nothing can be
 * done, on a screen that has just stopped telling the truth. It went missing in
 * about forty places, which is what a rule kept by discipline looks like at
 * scale.
 *
 * So it is not a rule here. Reading `error` is the only way to render anything
 * through this, and the order is the component's rather than the caller's.
 *
 * Where a page's own layout has to wrap each state differently — a table's
 * `colSpan` row, a card that keeps its header — use `QueryError` directly and
 * put it before the empty check by hand. This is for the common shape, which is
 * most of them.
 */
export function QueryState({
  query,
  loading,
  title,
  fallback,
  empty,
  isEmpty,
  children,
}: {
  query: QueryLike;
  /** The skeleton, shaped like the content. */
  loading: ReactNode;
  /** What could not be read — the subject, not a verdict on why. */
  title: string;
  /** Said when a refusal carried no message. Name what the read needs. */
  fallback: string;
  /** Rendered when the read succeeded and carried nothing. Omit where "nothing"
   *  is not a state this surface distinguishes. */
  empty?: ReactNode;
  /** Whether the answer was empty. Only consulted when `empty` is given. */
  isEmpty?: boolean;
  children: ReactNode;
}) {
  if (query.isLoading) return <>{loading}</>;
  if (query.error) {
    return (
      <QueryError
        title={title}
        error={query.error}
        fallback={fallback}
        onRetry={query.refetch ? () => void query.refetch?.() : undefined}
      />
    );
  }
  if (empty !== undefined && isEmpty) return <>{empty}</>;
  return <>{children}</>;
}
