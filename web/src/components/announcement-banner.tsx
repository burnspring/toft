import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, CircleAlert, Megaphone, X } from "lucide-react";
import { api } from "@/lib/api";
import { toneSurface, type Tone } from "@/lib/tone";
import type { Announcement, AnnouncementLevel } from "@/lib/api";

const DISMISS_KEY = "toft.announcement.dismissed";

/** An administrator's three levels, onto the product's tones. The icon and the
 * word do the work that colour alone cannot: an announcement bar is the one
 * surface a person meets before they have read anything else on the page. */
const levelStyles: Record<AnnouncementLevel, { tone: Tone; icon: ReactNode; label: string }> = {
  info: { tone: "info", icon: <Megaphone className="h-4 w-4" />, label: "Announcement" },
  // Not ⓘ: that glyph is the product's neutral information.
  warning: { tone: "attention", icon: <CircleAlert className="h-4 w-4" />, label: "Notice" },
  critical: { tone: "danger", icon: <AlertTriangle className="h-4 w-4" />, label: "Important" },
};

// Builds a stable dismissal token — distinct content or distinct publish time
// means the banner will resurface for every user.
function dismissToken(a: Announcement): string {
  return `${a.updated_at ?? ""}::${a.message}`;
}

export function AnnouncementBanner() {

  const [dismissed, setDismissed] = useState<string>(() => {
    try {
      return localStorage.getItem(DISMISS_KEY) ?? "";
    } catch {
      return "";
    }
  });

  /**
   * A query rather than a bare effect, so publishing one shows it.
   *
   * This fetched once on mount with no key, and the editor that publishes an
   * announcement is on another route — so an administrator who wrote one saw
   * nothing until they reloaded the page, and had no way to tell a banner that
   * had not arrived from one that had not been saved. The editor invalidates
   * `["announcement"]` now, and this is what hears it.
   */
  const { data: announcement } = useQuery({
    queryKey: ["announcement"],
    queryFn: api.getAnnouncement,
    retry: false,
  });

  if (!announcement || !announcement.message) return null;

  const token = dismissToken(announcement);
  const isCritical = announcement.level === "critical";
  // Critical banners cannot be dismissed.
  if (!isCritical && dismissed === token) return null;

  const style = levelStyles[announcement.level ?? "info"] ?? levelStyles.info;

  const handleDismiss = () => {
    try {
      localStorage.setItem(DISMISS_KEY, token);
    } catch {
      // ignore
    }
    setDismissed(token);
  };

  return (
    // A named region, not a live one: it is not news the reader just caused,
    // and the Layout remounts between `/` and every other route, so a live
    // banner was re-announced on every trip to Apps and back.
    <div
      role="region"
      aria-label="Announcement"
      className={`border-x-0 border-t-0 border-b ${toneSurface[style.tone]}`}
    >
      {/* `px-4 sm:px-6` like the header and `<main>`: at `px-6` the message
          started eight pixels right of the wordmark directly beneath it, on the
          one full-bleed bar in the product. */}
      <div className="mx-auto flex max-w-7xl items-start gap-3 px-4 py-2.5 text-sm sm:px-6">
        <span className="mt-0.5 shrink-0">{style.icon}</span>
        {/* Capped, now that this shares the header's sticky container: a long
            `whitespace-pre-wrap` critical message would otherwise hold that
            height for the whole session. */}
        <div className="max-h-24 flex-1 overflow-y-auto">
          <span className="font-medium">{style.label}:</span>{" "}
          <span className="whitespace-pre-wrap">{announcement.message}</span>
        </div>
        {!isCritical && (
          <button
            type="button"
            onClick={handleDismiss}
            className="shrink-0 rounded p-1 text-current/70 hover:bg-black/5 hover:text-current"
            aria-label="Dismiss announcement"
          >
            <X className="h-4 w-4" />
          </button>
        )}
      </div>
    </div>
  );
}
