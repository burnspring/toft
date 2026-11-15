import { Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import { copyToClipboard } from "@/lib/clipboard";

/**
 * The phrase a type-to-confirm dialog asks to be typed back — an app's slug, a
 * login, a store, an announcement's subject.
 *
 * Copyable, deliberately: the friction type-to-confirm buys is *reading the
 * name of the thing being destroyed*, not retyping a 40-character slug letter
 * by letter, and on Transfer ownership the phrase sat inside a `<Label>` —
 * `select-none` — so it could not even be selected. `select-all` makes one
 * click take the whole phrase, and the button beside it copies it.
 */
export function ConfirmPhrase({ text }: { text: string }) {
  return (
    <span className="inline-flex items-baseline gap-0.5 align-baseline">
      <span className="font-mono font-semibold text-foreground select-all">{text}</span>
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        className="self-center"
        aria-label={`Copy ${text}`}
        onClick={(e) => {
          // Inside a <Label> a click would otherwise move focus to the input
          // and, in a description, nothing would mind — stop it either way.
          e.preventDefault();
          void copyToClipboard(text, "text to confirm");
        }}
      >
        <Copy />
      </Button>
    </span>
  );
}
