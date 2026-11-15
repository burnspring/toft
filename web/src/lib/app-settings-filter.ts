import { createContext } from "react";

/**
 * Which sections of an app's Settings tab the filter has left standing.
 *
 * `null` means no filter is applied, which is not the same as an empty set —
 * one shows everything and the other shows nothing, and a context that
 * conflated them would blank the tab on mount.
 *
 * A context rather than a prop because the sections are ten separate
 * components, several of which decide for themselves whether they exist at
 * all (Storage is absent unless something is mapped, Gallery unless the app
 * came from one). Threading a `visible` prop through all of them would put the
 * filter's business in every one of their signatures; reading it in the one
 * wrapper they share puts it in none.
 */
export const VisibleSections = createContext<Set<string> | null>(null);
