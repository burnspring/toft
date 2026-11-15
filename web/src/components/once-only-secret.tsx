import { type ReactNode, useState } from "react";
import { copyToClipboard } from "@/lib/clipboard";
import { Copy } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";

/**
 * A secret the platform will never show again.
 *
 * Three surfaces mint one — an app key, a personal key, a deploy key — and
 * each had grown its own shape: two banners above a table and one dialog, with
 * three wordings of the same sentence and three ways of losing the thing.
 * A banner is lost to a tab switch, because the tab's state goes with it; the
 * dialog was lost to Escape. In every one of those the person is left holding
 * a credential that exists, is authorized, and can never be read — recoverable
 * only by revoking it and starting again.
 *
 * **So it is modal and it does not dismiss itself.** Escape and a click
 * outside do nothing, there is no close cross, and the only way out is the
 * button that says what pressing it means. That is a deliberate exception to
 * the dialog conventions and it earns it: this is the one dialog in the
 * product where dismissing by reflex destroys something.
 */
export function OnceOnlySecret({
  open,
  title,
  secret,
  label,
  children,
  description,
  onDismiss,
}: {
  open: boolean;
  /** The heading — normally naming the key that was just minted. */
  title: string;
  secret: string;
  /** The accessible name of the field holding it. */
  label: string;
  /** What this particular key is for, and anything the holder has to do next. */
  children?: ReactNode;
  /** Why it is shown once and what to do if it is lost — defaults to a key's
   *  sentence (hashed; revoke and create another). */
  description?: ReactNode;
  onDismiss: () => void;
}) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    if (await copyToClipboard(secret, label.toLowerCase())) setCopied(true);
  }

  return (
    <Dialog
      open={open}
      disablePointerDismissal
      // Every close request that is not the button below is refused. base-ui
      // reports the reason, and the only one this honours is `none` — the
      // programmatic close the button makes.
      onOpenChange={(next) => {
        if (!next) return;
      }}
    >
      <DialogContent className="sm:max-w-lg" showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>
            {description ??
              "Only a hash of it is stored, so this is the one time it can be shown. If it is lost, revoke it and create another."}
          </DialogDescription>
        </DialogHeader>

        <div className="flex items-center gap-2">
          <Input
            readOnly
            value={secret}
            aria-label={label}
            onFocus={(e) => e.target.select()}
            className="flex-1 font-mono text-xs"
          />
          <Button variant="outline" size="sm" onClick={() => void copy()}>
            <Copy className="mr-1.5 h-3.5 w-3.5" />
            Copy
          </Button>
        </div>

        {children}

        <DialogFooter className="items-center">
          <p className="mr-auto text-xs text-muted-foreground">
            This dialog stays until you close it here.
          </p>
          <Button size="sm" variant={copied ? "default" : "secondary"} onClick={onDismiss}>
            {copied ? "Done" : "I have copied it"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
