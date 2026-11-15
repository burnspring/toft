import { useCallback } from "react";
import { useSearchParams } from "react-router-dom";

/**
 * The URL's query as it stands *now*, not as of the last render.
 *
 * react-router's functional `setSearchParams` hands its updater the params of
 * the last render, so two setters called in one handler — a Clear resetting
 * search and a facet — each start from the same old URL and the second write
 * undoes the first. `history.replaceState` has already moved
 * `window.location` by the time the next setter runs, so reading it there
 * composes the writes.
 */
function currentParams(): URLSearchParams {
  return new URLSearchParams(window.location.search);
}

/**
 * A list's narrowing, carried in the URL.
 *
 * Search, status, facets and sort lived in component state, so a page that had
 * been narrowed to something could not be sent to anybody: no link went to
 * "Crashed", Back stepped out of the page rather than out of the last filter,
 * and a reload put a person back at the top of the whole list. On a page an
 * operator reaches for during an incident that is the difference between
 * pasting a link into a channel and describing which buttons to press.
 *
 * **Replace, not push.** Every chip toggled while reading is not a place
 * somebody wants to return to one Back at a time; the destination is the
 * *page*, and Back should leave it. The one exception nobody has asked for is
 * an undo of a filter, and Clear is that.
 *
 * The parameter names are part of the URL and therefore part of what a person
 * pastes into a ticket — short, lower case, and named for what the reader sees
 * rather than for the state variable behind it.
 */
export function useUrlParam(
  name: string,
  fallback = "",
): [string, (next: string) => void] {
  const [params, setParams] = useSearchParams();
  const value = params.get(name) ?? fallback;
  const set = useCallback(
    (next: string) => {
      setParams(
        () => {
          const out = currentParams();
          // The default is absent rather than spelled out: `?status=all` is
          // noise in a pasted link, and a URL that says nothing about a filter
          // is the one that keeps working when the default changes.
          if (next === fallback || next === "") out.delete(name);
          else out.set(name, next);
          return out;
        },
        { replace: true },
      );
    },
    [name, fallback, setParams],
  );
  return [value, set];
}

/** The same, for a facet that holds several values at once. */
export function useUrlParams(name: string): [string[], (next: string[]) => void] {
  const [params, setParams] = useSearchParams();
  const value = params.getAll(name);
  const set = useCallback(
    (next: string[]) => {
      setParams(
        () => {
          const out = currentParams();
          out.delete(name);
          next.forEach((v) => out.append(name, v));
          return out;
        },
        { replace: true },
      );
    },
    [name, setParams],
  );
  return [value, set];
}
