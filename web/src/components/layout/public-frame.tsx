import { ToftMark } from "@/components/toft-mark";
import { Link } from "react-router-dom";

/**
 * The frame around the three pages outside the `Layout` — the landing page,
 * `/login` and `/cli` — so the first screens anybody sees are one product, not
 * a marketing page and then a card floating on grey.
 *
 * One white surface, a sticky header carrying the wordmark and the page's one
 * action, a hairline under it, and a footer with the tagline. Translucent over
 * a blur rather than solid, so the bands passing under it are still felt.
 *
 * `footer={false}` is for a page that already says the tagline beside its own
 * heading and would otherwise say it twice on one screen. `homeLink={false}`
 * is for a page holding a decision — `/cli` — where the one way elsewhere on
 * the screen would be a way out of answering.
 */
export function PublicFrame({
  action,
  footer = true,
  homeLink = true,
  children,
}: {
  action?: React.ReactNode;
  footer?: boolean;
  homeLink?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-h-screen flex-col bg-card text-foreground">
      <a
        href="#main"
        className="sr-only rounded-md bg-primary px-3 py-2 text-sm font-medium text-primary-foreground focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50"
      >
        Skip to content
      </a>

      <header className="sticky top-0 z-40 border-b border-border/60 bg-card/85 backdrop-blur-md">
        <div className="mx-auto flex h-14 w-full max-w-6xl items-center justify-between px-4 sm:px-6">
          {homeLink ? (
            <Link to="/" className="rounded-sm">
              <Wordmark />
            </Link>
          ) : (
            <Wordmark />
          )}
          {action}
        </div>
      </header>

      <main id="main" className="flex flex-1 flex-col">
        {children}
      </main>

      {footer ? (
        <footer className="border-t border-border/60">
          <div className="mx-auto flex w-full max-w-6xl flex-col gap-2 px-4 py-8 text-sm text-muted-foreground sm:flex-row sm:items-center sm:justify-between sm:px-6">
            <Wordmark />
            <span>Where your organization’s own software lives</span>
          </div>
        </footer>
      ) : null}
    </div>
  );
}

function Wordmark() {
  return (
    <span className="flex items-center gap-2 font-semibold tracking-tight text-foreground">
      <ToftMark className="h-4.5 w-4.5 text-primary" />
      Toft
    </span>
  );
}
