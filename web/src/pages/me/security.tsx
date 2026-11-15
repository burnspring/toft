import { Disclosure } from "@/components/disclosure";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/relative-time";
import { Section } from "@/components/section";
import { QueryError } from "@/components/query-error";
import { EmptyState } from "@/components/empty-state";
import { useAuth } from "@/hooks/use-auth";
import { api } from "@/lib/api";
import type { MyAccount, MySignin } from "@/lib/api";
import { StatusBadge } from "@/components/status-badge";
import { FieldError } from "@/components/field-help";
import { describeDevice } from "@/lib/user-agent";
import type { Tone } from "@/lib/tone";

/**
 * The doors into this account: who can open them, who has, and how to shut
 * them all at once.
 */
export function MeSecurityTab({ account }: { account: MyAccount }) {
  return (
    <div className="space-y-6">
      <Section
        title="Sign-in providers"
        description="Your name, email and avatar come from the provider."
      >
        {account.identities.length === 0 ? (
          <EmptyState size="inline" title="This account signs in with a password only" />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Provider</TableHead>
                <TableHead className="w-full">Account</TableHead>
                <TableHead className="hidden w-[150px] @xl:table-cell">First seen</TableHead>
                <TableHead className="w-[150px]">Last used</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {account.identities.map((i) => (
                <TableRow key={`${i.provider_id}-${i.login}`}>
                  {/* Its own width: a provider name is short, and `max-w-0`
                      with no width collapsed it to "Dex (lo…". */}
                  <TableCell className="font-medium text-foreground">
                    {i.provider_name}
                  </TableCell>
                  <TableCell className="max-w-0 truncate text-sm text-muted-foreground">
                    {i.name ? `${i.name} · ` : ""}
                    {i.email || i.login || "—"}
                  </TableCell>
                  <TableCell className="hidden text-sm text-muted-foreground @xl:table-cell">
                    <RelativeTime iso={i.created_at} />
                  </TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {i.last_login_at ? <RelativeTime iso={i.last_login_at} /> : "Never"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>

      <RecentSignins />

      {/* Absent unless this account actually has a password. Exactly one does. */}
      {account.can_change_password && <PasswordSection />}

      <SignOutEverywhere />
    </div>
  );
}

/**
 * What the action words mean, in the voice of the person they happened to —
 * and what each one *is*.
 *
 * The column used to be plain text, so a refusal read in exactly the same ink
 * as a success. This is the page somebody opens because they are worried, and
 * the one row they came to find had no more weight than the twenty-seven around
 * it. A signing-in and a signing-out are records of ordinary things; a refusal
 * is the fact this page exists for.
 */
const SIGNIN_ACTION: Record<string, { label: string; tone: Tone }> = {
  "session.signin": { label: "Signed in", tone: "ok" },
  "session.signin_failed": { label: "Sign-in refused", tone: "danger" },
  "session.signout": { label: "Signed out", tone: "neutral" },
  "session.revoke_all": { label: "Ended all sessions", tone: "attention" },
};

/**
 * Recent sign-ins — the one security question a person can answer about
 * themselves and act on without an administrator.
 *
 * A keyhole, not a door, and the server is what makes it one: the caller's own
 * rows, the session family only, a fixed window and a fixed cap. The copy says
 * what those bounds are, because a list that silently stops at fifty reads as
 * "this is everything".
 */
/**
 * How many rows show before "Show all". The newest are what this page is
 * opened for; the server already sends the whole capped window, so the rest
 * is one click away with no request — paging furniture for a list of at most
 * fifty would be more chrome than the rows it hides.
 */
const SIGNINS_SHOWN = 10;

function RecentSignins() {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["my-sign-ins"],
    queryFn: api.getMySignins,
    retry: false,
  });
  const [showAll, setShowAll] = useState(false);
  const rows = data?.sign_ins ?? [];
  const shown = showAll ? rows : rows.slice(0, SIGNINS_SHOWN);

  return (
    <Section
      title="Recent sign-ins"
      description={
        data
          ? `The last ${data.days} days, up to ${data.limit}.`
          : undefined
      }
    >
      {error ? (
        <QueryError
          title="Your sign-in history could not be read"
          error={error}
          fallback="Reload the page. If it keeps failing, tell an administrator."
          onRetry={() => void refetch()}
        />
      ) : isLoading ? (
        <div className="space-y-2 p-4">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-8 w-full" />)}
        </div>
      ) : !data || data.sign_ins.length === 0 ? (
        <EmptyState size="inline" title="Nothing recorded in this period" />
      ) : (
        <Table>
          <TableHeader>
            {/* 500px of fixed columns and nothing yielding, on a page a
                person reaches from a phone the moment they suspect an
                account. When and Outcome are the answer and stay; the address
                yields and is restated under the device beside it, and Device
                takes the leftover explicitly rather than letting auto layout
                share the slack evenly with three columns that asked for a
                width. */}
            <TableRow className="hover:bg-transparent">
              <TableHead className="w-[170px]">When</TableHead>
              <TableHead className="w-[180px]">Outcome</TableHead>
              <TableHead className="hidden w-[150px] @xl:table-cell">From</TableHead>
              <TableHead className="w-full">Device</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((s, i) => <SigninRow key={i} row={s} />)}
          </TableBody>
        </Table>
      )}
      {!error && !isLoading && rows.length > SIGNINS_SHOWN && !showAll && (
        <div className="border-t border-border/60 py-1.5">
          <Button variant="ghost" size="sm" onClick={() => setShowAll(true)}>
            Show all ({rows.length})
          </Button>
        </div>
      )}
    </Section>
  );
}

function SigninRow({ row }: { row: MySignin }) {
  const outcome = SIGNIN_ACTION[row.action];
  const device = describeDevice(row.user_agent);
  return (
    <TableRow>
      <TableCell className="text-sm text-muted-foreground">
        <RelativeTime iso={row.occurred_at} exact />
      </TableCell>
      <TableCell>
        {outcome ? (
          <StatusBadge status={row.action} tone={outcome.tone} label={outcome.label} />
        ) : (
          <span className="text-sm text-foreground">{row.action}</span>
        )}
      </TableCell>
      {/* Monospace because it is compared character by character, and that is
          the whole use of the column. On an install behind a proxy every row
          reads as the proxy — honest, if not always useful. */}
      <TableCell className="hidden font-mono text-xs text-muted-foreground @xl:table-cell">
        {row.ip || "—"}
      </TableCell>
      {/* The device as a person would name it, with the string it came from on
          the row for anybody who wants it. See `lib/user-agent.ts`. */}
      <TableCell className="text-xs text-muted-foreground">
        {/* The address, at the width its column goes. */}
        <span className="mb-0.5 block font-mono text-2xs @xl:hidden">{row.ip || "—"}</span>
        {device ? (
          <>
            <span className={device.known ? undefined : "font-mono"}>{device.label}</span>
            {/* The string it came from, reachable rather than hovered — and
                only where the label is not already the whole of it. */}
            {device.label !== device.raw && (
              <Disclosure label="Device details" size="xs">
                <p className="font-mono text-2xs break-all">{device.raw}</p>
              </Disclosure>
            )}
          </>
        ) : (
          "—"
        )}
      </TableCell>
    </TableRow>
  );
}

/**
 * The password form, for the one account that has a password.
 *
 * It posts username, current password and new password, and reports the API's
 * own 422s verbatim rather than re-implementing the policy in TypeScript —
 * the length rule and the must-differ rule live in `internal/password`, and a
 * second copy here would be the one that goes stale.
 */
function PasswordSection() {
  const { user } = useAuth();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  /**
   * Typed twice, because there is no second chance to find out.
   *
   * This is the built-in administrator's password — the way in that needs no
   * provider — and it is a `type="password"` field, so a typo is invisible
   * while it is typed and the save that follows ends every other session. The
   * account is then locked out of an install whose whole point is that it can
   * be recovered from the machine.
   */
  const [confirm, setConfirm] = useState("");
  const [saving, setSaving] = useState(false);

  const mismatch = confirm !== "" && confirm !== next;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!user || !current || !next || next !== confirm) return;
    setSaving(true);
    try {
      await api.changePassword(user.login, current, next);
      setCurrent("");
      setNext("");
      setConfirm("");
      toast.success("Password changed — your other sessions have ended");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t change the password");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Section
      title="Password"
      description="Changing it signs out your other sessions."
    >
      <form className="space-y-4 px-4 py-4" onSubmit={submit}>
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="current-password">Current password</Label>
            <Input
              id="current-password"
              type="password"
              autoComplete="current-password"
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="new-password">New password</Label>
            <Input
              id="new-password"
              type="password"
              autoComplete="new-password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="confirm-password">Confirm new password</Label>
            <Input
              id="confirm-password"
              type="password"
              autoComplete="new-password"
              value={confirm}
              aria-invalid={mismatch}
              aria-describedby={mismatch ? "confirm-password-error" : undefined}
              onChange={(e) => setConfirm(e.target.value)}
            />
            {mismatch && (
              <FieldError field="confirm-password">
                The passwords don’t match.
              </FieldError>
            )}
          </div>
        </div>
        <Button type="submit" size="sm" disabled={saving || !current || !next || next !== confirm}>
          {saving ? "Changing…" : "Change password"}
        </Button>
      </form>
    </Section>
  );
}

/**
 * One button over a mechanism that has always existed.
 *
 * A dialog rather than type-to-confirm: it is not irreversible — you sign in
 * again — but it is not what the button says either, because it ends the
 * session pressing it, which no other reversible action on this platform does.
 * So the dialog says that in a sentence and names what survives.
 */
function SignOutEverywhere() {
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);

  async function revoke() {
    setPending(true);
    try {
      await api.revokeMySessions();
      // The server already cleared this browser's cookies; a full navigation
      // is what drops the client's cached user with them. `signed_out`, so a
      // single-provider install does not forward straight back in.
      window.location.href = "/login?signed_out=1";
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t end your sessions");
      setPending(false);
    }
  }

  return (
    <Section
      title="Sign out everywhere"
      description="Use this if a laptop or phone is lost."
    >
      <div className="px-4 py-4">
        <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
          Sign out everywhere
        </Button>
      </div>

      <Dialog open={open} onOpenChange={(o) => !pending && setOpen(o)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Sign out of every session?</DialogTitle>
            <DialogDescription>
              Every device is signed out, this one included. Your API keys and apps keep working.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={pending} onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button variant="destructive" disabled={pending} onClick={revoke}>
              {pending ? "Signing out…" : "Sign out everywhere"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Section>
  );
}
