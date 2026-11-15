import { AlertTriangle } from "lucide-react";

import { EmptyState } from "@/components/empty-state";
import { Button } from "@/components/ui/button";
import { isRefusal, statusOf } from "@/lib/api";

/**
 * A read that did not come back, said as itself.
 *
 * The failure this prevents is the one where a page renders its *empty* state
 * because a fetch failed — "No apps found. Deploy your first app to get
 * started." while the API is down. Empty and broken are different facts, and
 * the difference matters most during an incident, which is exactly when
 * somebody is reading this page.
 *
 * **Refused and broken are two more different facts**, and until `ApiError`
 * carried a status no page could tell them apart. Thirteen admin pages resolved
 * that by asserting "Administrators only" for every failure, which is the worst
 * available sentence: it tells an administrator watching their own install fall
 * over that they lack a permission they hold, and sends them to ask for access
 * instead of to the logs. So:
 *
 * - **A refusal** — 401, 403, 404 — is the server answering, and its own words
 *   are the answer. `fallback` is for the refusal that carried none, and should
 *   name the permission or capability the read needs rather than apologise.
 *   There is no Retry, because nothing about pressing it again would differ.
 * - **Anything else** broke. The server's message still leads where there is
 *   one, but the page never claims to know it was permission, and Retry is
 *   offered wherever the caller can refetch.
 *
 * The admin pages invented this shape independently about a dozen times. This
 * is that shape, named.
 */
export function QueryError({
  title,
  error,
  fallback,
  onRetry,
}: {
  /** The subject that could not be read — "The activity log", not a verdict on
   *  why. This component decides what to say about the why. */
  title: string;
  error: unknown;
  /** Said when a *refusal* carried no message. Name what the read needs. */
  fallback: string;
  /** Where the caller can ask again. Offered only when the failure was not a
   *  refusal. Usually a query's `refetch`. */
  onRetry?: () => void;
}) {
  const message = error instanceof Error && error.message ? error.message : "";
  const refused = isRefusal(error);
  const status = statusOf(error);

  // A broken read still says it was not a refusal, in the words a person uses.
  const description = refused
    ? message || fallback
    : message
      ? `${message} This is a server error, not a permission problem.`
      : status
        ? `The server answered ${status}. This is a server error, not a permission problem.`
        : "The server didn’t answer. It may be restarting, or this browser may be offline.";

  return (
    <EmptyState
      // A read that failed is news, and it is the one thing that replaces a
      // skeleton without being announced. See EmptyState's `live`.
      live
      icon={<AlertTriangle className="h-6 w-6" />}
      title={title}
      description={description}
      action={
        !refused && onRetry ? (
          <Button variant="outline" size="sm" onClick={onRetry}>
            Retry
          </Button>
        ) : undefined
      }
    />
  );
}
