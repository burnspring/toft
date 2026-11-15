import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { OctagonAlert } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { toneSurface } from "@/lib/tone";
import { isUnreachable, probeConnection, subscribeConnection } from "@/lib/connection";

const POLL_MS = 5000;

/**
 * The install has stopped answering — said above the header, on every page,
 * the moment any request finds out (`lib/connection.ts`).
 *
 * Without it a tab kept rendering its cached copy and an outage was invisible
 * until somebody reloaded. While it shows, it asks again every few seconds;
 * when an answer comes back it goes, and every query on screen is refetched,
 * because what the page holds is exactly as old as the outage.
 *
 * Live, unlike the announcement bar beside it: it appears because something
 * just happened, not because a page was opened.
 */
export function ConnectionBanner() {
  const down = useSyncExternalStore(subscribeConnection, isUnreachable);
  const queryClient = useQueryClient();
  const [checking, setChecking] = useState(false);
  const wasDown = useRef(false);

  useEffect(() => {
    if (down) {
      wasDown.current = true;
      const timer = window.setInterval(() => void probeConnection(), POLL_MS);
      return () => window.clearInterval(timer);
    }
    if (wasDown.current) {
      wasDown.current = false;
      void queryClient.invalidateQueries();
      toast.success("Connected again");
    }
  }, [down, queryClient]);

  if (!down) return null;

  const retry = async () => {
    setChecking(true);
    try {
      await probeConnection();
    } finally {
      setChecking(false);
    }
  };

  return (
    <div
      role="status"
      aria-live="polite"
      className={`border-x-0 border-t-0 border-b ${toneSurface.danger}`}
    >
      <div className="mx-auto flex max-w-7xl items-center gap-3 px-4 py-2 text-sm sm:px-6">
        <OctagonAlert className="h-4 w-4 shrink-0" aria-hidden />
        <p className="max-w-prose flex-1">
          <span className="font-medium">Can’t reach this install.</span>{" "}
          What’s on screen may be out of date. Checking again every few seconds.
        </p>
        <Button size="sm" variant="outline" onClick={retry} disabled={checking}>
          {checking ? "Checking…" : "Retry"}
        </Button>
      </div>
    </div>
  );
}
