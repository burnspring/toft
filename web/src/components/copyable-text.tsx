import { Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { copyToClipboard } from "@/lib/clipboard";
import { cn } from "@/lib/utils";

/**
 * A literal somebody copies whole — a command, a block of toft.toml — with its
 * copy button in the corner. The same box `StorageUsage` draws.
 */
export function CopyableText({
  text,
  subject,
  className,
}: {
  text: string;
  /** A lower-case noun phrase for the toast and the button's name: "command". */
  subject: string;
  className?: string;
}) {
  return (
    <div className={cn("relative", className)}>
      <pre className="overflow-x-auto rounded-md bg-muted px-3 py-2.5 pr-10 font-mono text-xs leading-relaxed whitespace-pre-wrap break-all text-foreground">
        {text}
      </pre>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="absolute right-1.5 top-1.5 h-7 w-7"
        aria-label={`Copy the ${subject}`}
        onClick={() => void copyToClipboard(text, subject)}
      >
        <Copy className="h-3.5 w-3.5" />
      </Button>
    </div>
  );
}
