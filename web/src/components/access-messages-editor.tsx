import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { api, type PlatformSettings } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Section } from "@/components/section";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { QueryState } from "@/components/query-state";
import { Textarea } from "@/components/ui/textarea";
import { FieldError, FieldHelp } from "@/components/field-help";
import { SETTING } from "@/lib/settings-index";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { toneText } from "@/lib/tone";
import { cn, fieldErrorId } from "@/lib/utils";

const MAX_LEN = 600;

/**
 * What people are told when this platform turns them away.
 *
 * Three refusals reach someone who authenticated perfectly well: their account
 * is locked, they have no account and no provider will create one, or their
 * account exists and nobody has activated it yet. Toft can only say *that*
 * it refused; the organization is the one that knows what the person should do
 * next — which queue to ask, which policy applies, whether it is temporary. So
 * the wording is an administrator's.
 *
 * Blank restores the default rather than storing silence: a refusal with no
 * explanation is the one outcome worth preventing outright.
 *
 * The last box is the odd one and is here anyway: it is not a refusal at all
 * but the sentence somebody reads when they are let *in*. It belongs beside
 * them because it is the same act — this install saying, in its own words, what
 * has happened to your account here — and separating it would have made a card
 * of one field on a page about exactly this.
 */
export function AccessMessagesEditor() {
  const queryClient = useQueryClient();
  const settingsQuery = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
  });
  const settings = settingsQuery.data;

  // Null means "not edited yet"; the stored value shows through until then.
  // This card is always mounted, so it cannot seed its state from a `key`.
  const [locked, setLocked] = useState<string | null>(null);
  const [noAccount, setNoAccount] = useState<string | null>(null);
  const [appLocked, setAppLocked] = useState<string | null>(null);
  const [pendingMsg, setPendingMsg] = useState<string | null>(null);
  const [activated, setActivated] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const lockedValue = locked ?? settings?.locked_account_message ?? "";
  const noAccountValue = noAccount ?? settings?.no_account_message ?? "";
  const appLockedValue = appLocked ?? settings?.app_locked_message ?? "";
  const pendingValue = pendingMsg ?? settings?.pending_activation_message ?? "";
  const activatedValue = activated ?? settings?.account_activated_message ?? "";
  const dirty =
    (locked !== null && locked !== settings?.locked_account_message) ||
    (noAccount !== null && noAccount !== settings?.no_account_message) ||
    (appLocked !== null && appLocked !== settings?.app_locked_message) ||
    (pendingMsg !== null && pendingMsg !== settings?.pending_activation_message) ||
    (activated !== null && activated !== settings?.account_activated_message);
  // A paste over the limit is kept and flagged, never cut: truncating it lost
  // the tail with nothing on screen but a counter reading 600/600.
  const tooLong = [lockedValue, noAccountValue, appLockedValue, pendingValue, activatedValue].some(
    (v) => v.length > MAX_LEN,
  );

  async function save() {
    setSaving(true);
    try {
      const result = await api.updatePlatformSettings({
        locked_account_message: lockedValue,
        no_account_message: noAccountValue,
        app_locked_message: appLockedValue,
        pending_activation_message: pendingValue,
        account_activated_message: activatedValue,
      });
      // The server substitutes the default for a cleared box, so the form shows
      // what was stored rather than what was typed.
      setLocked(result.settings.locked_account_message);
      setNoAccount(result.settings.no_account_message);
      setAppLocked(result.settings.app_locked_message);
      setPendingMsg(result.settings.pending_activation_message);
      setActivated(result.settings.account_activated_message);
      queryClient.setQueryData(["platform-settings"], (old: PlatformSettings | undefined) =>
        old ? { ...old, ...result.settings } : old,
      );
      queryClient.invalidateQueries({ queryKey: ["platform-settings"] });
      // The login page reads these from its own endpoint.
      queryClient.invalidateQueries({ queryKey: ["login-options"] });
      toast.success("Saved");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t save the messages");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Section
      title="Access messages"
      description="Plain text. Leave a box empty to restore the standard wording."
    >
      <div className="space-y-4 p-4">
        <QueryState
          query={settingsQuery}
          title="These messages could not be read"
          fallback={ADMIN_ONLY_FALLBACK}
          loading={
            <>
              <Skeleton className="h-20 w-full" />
              <Skeleton className="h-20 w-full" />
              <Skeleton className="h-20 w-full" />
              <Skeleton className="h-20 w-full" />
              <Skeleton className="h-20 w-full" />
            </>
          }
        >
          <>
            <MessageField
              id="locked-message"
              label={SETTING.locked_account_message.label}
              value={lockedValue}
              onChange={setLocked}
              help="Shown when a locked account tries to sign in."
            />
            <MessageField
              id="no-account-message"
              label={SETTING.no_account_message.label}
              value={noAccountValue}
              onChange={setNoAccount}
              help="Shown when someone signs in but has no account here."
            />
            {/* The third refusal: the account exists and is in a queue. Its own
                box rather than a shade of "no account", because "there is
                nothing here for you" and "wait" are different instructions, and
                only one of them is worth acting on. */}
            <MessageField
              id="pending-activation-message"
              label={SETTING.pending_activation_message.label}
              value={pendingValue}
              onChange={setPendingMsg}
              help="Shown at each sign-in until an administrator activates the account."
            />
            {/* The fourth of the family and the only one that refuses somebody
                at an app they are otherwise entitled to open. It is the
                install-wide default only — an administrator locking a
                particular app may write something better, and usually should.  */}
            <MessageField
              id="app-locked-message"
              label={SETTING.app_locked_message.label}
              value={appLockedValue}
              onChange={setAppLocked}
              help="Shown when someone opens a locked app that has no message of its own."
            />
            {/* The one message here somebody is glad to read, and the only one
                that leaves by mail rather than appearing on a page. */}
            <MessageField
              id="account-activated-message"
              label={SETTING.account_activated_message.label}
              value={activatedValue}
              onChange={setActivated}
              help="Emailed when you activate an account, if email is set up."
            />
            {/* Left, like the banner's and the knowledge base's saves on the
                same page — three saves aligned three ways read as three forms
                that work differently. */}
            <div className="flex">
              <Button size="sm" onClick={save} disabled={saving || !dirty || tooLong}>
                {saving ? "Saving…" : "Save messages"}
              </Button>
            </div>
          </>
        </QueryState>
      </div>
    </Section>
  );
}

function MessageField({
  id,
  label,
  value,
  onChange,
  help,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  help: string;
}) {
  const over = value.length > MAX_LEN;
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id} className="text-sm font-medium">
        {label}
      </Label>
      <Textarea
        id={id}
        value={value}
        aria-invalid={over}
        aria-describedby={over ? fieldErrorId(id) : undefined}
        onChange={(e) => onChange(e.target.value)}
        rows={3}
      />
      <div className="flex items-start justify-between gap-4">
        <FieldHelp>{help}</FieldHelp>
        <span
          className={cn("shrink-0 text-xs", over ? toneText.danger : "text-muted-foreground")}
        >
          {value.length}/{MAX_LEN}
        </span>
      </div>
      <FieldError field={id}>{over ? `Shorten this to ${MAX_LEN} characters.` : null}</FieldError>
    </div>
  );
}
