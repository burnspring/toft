import { createContext } from "react";

/**
 * The term a page's filter is currently narrowed to.
 *
 * A `SettingRow` reads it to decide whether to open its own "Why this matters"
 * disclosure — a row whose only match is inside folded-away reasoning would
 * otherwise look like a mistake. Empty everywhere but inside a filtered list,
 * which is why it is a context rather than a prop: the pages that render one
 * row have no filter at all.
 */
export const FilterTerm = createContext("");
