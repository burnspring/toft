import { Component, type ErrorInfo, type ReactNode } from "react";
import { useLocation } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/notice";
import { copyToClipboard } from "@/lib/clipboard";

/**
 * The last line of defence: a render error anywhere under here draws a page
 * instead of a blank screen.
 *
 * There was no boundary at all before this, which on a single-page app means a
 * thrown error in one cell of one table unmounts the entire document and leaves
 * white. On an operations console that is the worst available failure — the
 * moment somebody most needs this UI is the moment something is misbehaving,
 * and "the whole site is blank" tells them nothing and offers them nothing.
 *
 * What it shows is shaped by who is looking. This is an internal tool whose
 * users are frequently the people who can fix it, so the message is not
 * "something went wrong" and a shrug: the error and its stack are on the page,
 * behind a disclosure, with a button that copies them. A bug report that
 * arrives with a stack in it is a different class of bug report.
 *
 * **It resets on navigation.** A boundary that latches is a boundary that
 * turns one broken page into a broken session — every link still routes, and
 * every destination still renders the error from the page you have left. The
 * wrapper below passes the pathname down, and `componentDidUpdate` clears a
 * caught error when it changes, so moving is always a way out.
 *
 * It clears rather than remounts, deliberately. A React `key` would also
 * discard a healthy subtree on `/apps/:id/:tab` and on every folder step of
 * `/storage/:slug/*`, which are pathname changes inside one mounted route.
 */

interface Props {
  children: ReactNode;
  /** Changing this clears a caught error; a healthy subtree is left mounted. */
  resetKey: string;
}

interface State {
  error: Error | null;
  info: ErrorInfo | null;
}

class Boundary extends Component<Props, State> {
  state: State = { error: null, info: null };

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  componentDidUpdate(prev: Props) {
    if (this.state.error && prev.resetKey !== this.props.resetKey) {
      this.setState({ error: null, info: null });
    }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    this.setState({ info });
    // The console is where a developer looks first, and React's own overlay is
    // absent in a production build.
    console.error("Unhandled render error", error, info);
  }

  render() {
    const { error, info } = this.state;
    if (!error) return this.props.children;

    const details = [
      error.stack ?? `${error.name}: ${error.message}`,
      info?.componentStack ? `\nComponent stack:${info.componentStack}` : "",
      `\nPage: ${window.location.pathname}${window.location.search}`,
    ].join("");

    return (
      <div className="mx-auto max-w-2xl py-12">
        <Notice
          tone="danger"
          size="md"
          title="This page stopped working"
          action={
            <Button size="sm" variant="outline" onClick={() => window.location.reload()}>
              Reload
            </Button>
          }
        >
          <p>
            This is a bug in Toft, not something you did. The rest of Toft still works — go
            to another page or reload this one.
          </p>
          <p className="mt-2 font-mono">{error.message || error.name}</p>
        </Notice>

        <details className="mt-4 rounded-lg border border-border/60 bg-card p-4 text-xs shadow-sm">
          <summary className="cursor-pointer font-medium text-foreground">
            Technical details
          </summary>
          <pre
            tabIndex={0}
            role="region"
            aria-label="Error detail"
            className="mt-3 max-h-64 overflow-auto focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none rounded bg-muted p-3 font-mono text-2xs whitespace-pre-wrap text-muted-foreground"
          >
            {details}
          </pre>
          <Button
            size="sm"
            variant="outline"
            className="mt-3"
            onClick={() => void copyToClipboard(details, "details")}
          >
            Copy details
          </Button>
        </details>
      </div>
    );
  }
}

export function ErrorBoundary({ children }: { children: ReactNode }) {
  const location = useLocation();
  return <Boundary resetKey={location.pathname}>{children}</Boundary>;
}
