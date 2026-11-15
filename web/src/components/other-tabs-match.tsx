import { Link } from "react-router-dom";

import {
  APP_TAB_LABEL,
  appSettingHref,
  searchAppSettings,
  type AppTab,
} from "@/lib/settings-index";

/**
 * Where else on *this app* the word being filtered for is a setting.
 *
 * `OtherPagesMatch` one level down, and it exists for the same reason: a
 * filter that can only see its own surface answers "no such setting" when the
 * honest answer is "not on this tab". The concrete case is `visibility` — the
 * decision about who may open this app's URL at all, typed into the Settings
 * tab, which does not hold it and until now could only deny it existed.
 *
 * Nothing is rendered when there is nothing to say.
 */
export function OtherTabsMatch({
  appId,
  query,
  tab,
  className,
}: {
  appId: string;
  query: string;
  /** The tab doing the filtering. Its own matches are not "elsewhere". */
  tab: AppTab;
  className?: string;
}) {
  const matches = searchAppSettings(query, tab);
  if (matches.length === 0) return null;

  return (
    <p className={`text-sm text-muted-foreground ${className ?? ""}`} aria-live="polite">
      {matches.length} setting{matches.length === 1 ? "" : "s"} on another tab of this app
      {matches.length === 1 ? " matches" : " match"} “{query}”:{" "}
      {matches.map((entry, i) => (
        <span key={entry.id}>
          {i > 0 && ", "}
          <Link
            to={appSettingHref(appId, entry)}
            className="rounded-sm text-primary hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50"
          >
            {entry.label}
          </Link>{" "}
          <span className="text-xs">({APP_TAB_LABEL[entry.tab]})</span>
        </span>
      ))}
      .
    </p>
  );
}
