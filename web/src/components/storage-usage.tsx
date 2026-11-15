import { useState } from "react";
import { Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { FieldHelp } from "@/components/field-help";
import type { StorageAccess, StorageKind } from "@/lib/api";
import { copyToClipboard } from "@/lib/clipboard";
import {
  USAGE_LANGUAGES, USAGE_LANGUAGE_LABEL, cliExample, rememberUsageTab, rememberedUsageTab,
  sdkExample, type UsageTab,
} from "@/lib/storage-usage";
import { cn } from "@/lib/utils";

type Tab = UsageTab;

/**
 * How to use this storage, with its own name in every line.
 *
 * One panel wherever somebody meets a resource they can use — the storage
 * browser, their own Storage tab, the administrator's resource page and the
 * app's Storage block — so the way in is next to the thing rather than in a
 * manual that says `reports` about a store called `sales-data`.
 *
 * The language choice is remembered for the page's life only
 * (`rememberUsageTab`): somebody who picked R on one row wants R on the next,
 * and it is a convenience that needs no storage of its own.
 */

export function StorageUsage({
  name, kind, folder, access, cli = true, sdk = true, className,
}: {
  name: string;
  kind: StorageKind;
  /** The folder being looked at, or the one an attachment names. */
  folder?: string;
  /** This reader's level here, when known; it trims the terminal commands. */
  access?: StorageAccess;
  /** Whether people can reach it from a terminal at all. */
  cli?: boolean;
  /** Whether to show app code. */
  sdk?: boolean;
  className?: string;
}) {
  const tabs: Tab[] = [...(cli ? (["cli"] as Tab[]) : []), ...(sdk ? USAGE_LANGUAGES : [])];
  const [tab, setTab] = useState<Tab>(() =>
    rememberedUsageTab(tabs) ?? tabs[0],
  );
  if (tabs.length === 0) return null;
  const current = tabs.includes(tab) ? tab : tabs[0];

  const text =
    current === "cli"
      ? cliExample({ name, folder, access })
      : sdkExample({ name, folder, kind, language: current });

  function choose(next: Tab) {
    rememberUsageTab(next);
    setTab(next);
  }

  return (
    <div className={cn("space-y-2", className)}>
      {/* Pressed buttons, not tabs: there is no tabpanel or arrow-key
          movement here, and announcing tabs that don't behave as tabs is
          worse than a plain group. */}
      <div className="flex flex-wrap items-center gap-1" role="group" aria-label="Show usage for">
        {tabs.map((t) => (
          <button
            key={t}
            type="button"
            aria-pressed={t === current}
            onClick={() => choose(t)}
            className={cn(
              "h-7 rounded-md px-2.5 text-xs font-medium text-muted-foreground hover:bg-accent hover:text-foreground",
              t === current && "bg-accent text-foreground",
            )}
          >
            {t === "cli" ? "Terminal" : USAGE_LANGUAGE_LABEL[t]}
          </button>
        ))}
      </div>
      <div className="relative">
        <pre className="overflow-x-auto rounded-md bg-muted px-3 py-2.5 pr-10 font-mono text-xs leading-relaxed text-foreground">
          {text}
        </pre>
        <Button
          variant="ghost"
          size="icon"
          className="absolute right-1.5 top-1.5 h-7 w-7"
          aria-label={current === "cli" ? "Copy commands" : "Copy code"}
          onClick={() => void copyToClipboard(text, current === "cli" ? "commands" : "code")}
        >
          <Copy className="h-3.5 w-3.5" />
        </Button>
      </div>
      <FieldHelp>
        {current === "cli" ? (
          <>
            With the <code className="font-mono">toft</code> CLI, signed in as yourself
            {access ? "." : ", once you hold a grant here."}
          </>
        ) : (
          <>In the code of an app this storage is attached to.</>
        )}
      </FieldHelp>
    </div>
  );
}
