import { useEffect } from "react";

/** The suffix every page carries. One install, many browser tabs. */
const SUFFIX = "Toft";

/**
 * The landing page's whole title, the one page called without one. It is what
 * a bookmark and a search result show, so it says what Toft does; it matches
 * `<title>` in `index.html`, which is all a link preview ever reads.
 */
const HOME_TITLE = "Toft — apps your teams build, behind your sign-in";

/**
 * What the browser tab, the history entry, and the screen reader call this
 * page.
 *
 * Every page in this app was called "Toft" — the value in `index.html`,
 * never touched again, because a single-page app changes its route without
 * changing its document. Three things break as a result, and all three are
 * things people actually do with an operations console:
 *
 * - **Tabs.** Somebody comparing two apps, or an app against Admin → Activity,
 *   has two identical tabs and has to click one to find out which.
 * - **History.** Ctrl-Shift-T and the back-button dropdown list the same word
 *   over and over, so returning to a page means guessing.
 * - **Screen readers.** A route change in an SPA replaces the DOM without a
 *   navigation, and the document title is the one thing that would otherwise
 *   announce where you have arrived. Without it the page silently becomes a
 *   different page.
 *
 * `PageHeader` calls this with its own title, so the twenty-seven pages that
 * use it are covered by the declaration they already make. The four that draw
 * their own headings — an app, a person, sign-in, and a bad address — call it
 * directly.
 */
export function useDocumentTitle(title?: string) {
  useEffect(() => {
    document.title = title ? `${title} · ${SUFFIX}` : HOME_TITLE;
    // Restoring on unmount would flash the bare suffix between two pages, and
    // the next page sets its own on the same commit.
  }, [title]);
}
