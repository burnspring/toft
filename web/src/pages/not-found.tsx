import { Link } from "react-router-dom";
import { Compass } from "lucide-react";
import { EmptyState } from "@/components/empty-state";
import { buttonVariants } from "@/components/ui/button-variants";
import { useDocumentTitle } from "@/hooks/use-document-title";

/**
 * A URL this app has no page for.
 *
 * Without a `*` route, React Router matched nothing and rendered the chrome
 * around an empty `<main>` — a header, a nav, and a blank rectangle, which
 * reads as a page that failed to load rather than an address that does not
 * exist. The two need different reactions from the person looking at them.
 *
 * The copy points at the two things a mistyped or stale Toft URL is
 * usually reaching for. An app link that has rotted is by far the most common:
 * apps are addressed by id, and ids outlive nothing.
 */
export function NotFoundPage() {
  useDocumentTitle("Page not found");

  return (
    <EmptyState
      // This page has no other heading — `Layout` contributes none — so the
      // whole outline was one h3 under nothing. Here the empty state *is* the
      // page.
      as="h1"
      icon={<Compass className="h-6 w-6" />}
      title="There is no page at this address"
      description="The link may be out of date — an app that was deleted, or a page that has moved."
      action={
        <div className="flex items-center gap-2">
          <Link to="/" className={buttonVariants({ size: "sm" })}>
            Go to Apps
          </Link>
          <button
            className={buttonVariants({ variant: "outline", size: "sm" })}
            onClick={() => window.dispatchEvent(new CustomEvent("open-command-palette"))}
          >
            Search
          </button>
        </div>
      }
    />
  );
}
