import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

/**
 * The one role change that confirms: making somebody an administrator.
 *
 * Every other role change acts on the pick and toasts — reversible and
 * audited. This one hands over the install's top role from a select in a
 * dense table, where one stray pick is all it takes, so it is a plain confirm
 * that says what the role reaches. Demoting stays direct: it narrows access,
 * and the server refuses the last administrator's.
 */
export function MakeAdminDialog({
  login,
  open,
  pending,
  onConfirm,
  onOpenChange,
}: {
  login: string;
  open: boolean;
  pending: boolean;
  onConfirm: () => void;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={(o) => !pending && onOpenChange(o)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Make {login} an administrator?</DialogTitle>
          <DialogDescription>
            They’ll be able to change every app, account and setting on this install.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={pending} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button disabled={pending} onClick={onConfirm}>
            {pending ? "Saving…" : "Make administrator"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
