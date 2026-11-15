import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal, Plus } from "lucide-react";
import { toast } from "sonner";

import { api } from "@/lib/api";
import type { AuthProvider, ExchangedKey, TrustedIssuer, TrustedIssuerInput } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { Section, SectionHeading } from "@/components/section";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import { FieldError, FieldHelp } from "@/components/field-help";
import { fieldErrorId } from "@/lib/utils";
import { Code } from "@/components/code";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

/**
 * Trusted issuers (ADR §157): who may vouch for a person, so that a caller
 * holding that person's identity token — Posit Workbench's rsconnect, a
 * Garth session — gets a short-lived deploy key instead of a long-lived
 * personal one. Nothing is trusted until a row exists here.
 *
 * The live keys sit under the issuers, because the question somebody brings
 * here after an incident is *what did that issuer's word buy, and is it still
 * working*.
 */
export function TrustedIssuersEditor({ providers }: { providers: AuthProvider[] }) {
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<TrustedIssuer | "new" | null>(null);
  const [removing, setRemoving] = useState<TrustedIssuer | null>(null);
  const [revoking, setRevoking] = useState<ExchangedKey | null>(null);

  const issuers = useQuery({ queryKey: ["trusted-issuers"], queryFn: api.listTrustedIssuers, retry: false });
  const keys = useQuery({ queryKey: ["exchanged-keys"], queryFn: api.listExchangedKeys, retry: false });
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["trusted-issuers"] });
    queryClient.invalidateQueries({ queryKey: ["exchanged-keys"] });
  };
  const ssoProviders = providers.filter((p) => p.kind !== "password");
  const providerName = (id: string) => ssoProviders.find((p) => p.id === id)?.display_name ?? id;

  async function toggle(t: TrustedIssuer, enabled: boolean) {
    try {
      await api.updateTrustedIssuer(t.id, { enabled });
      toast.success(enabled ? `“${t.display_name}” can vouch for people again` : `“${t.display_name}” vouches for nobody now`);
      refresh();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t switch “${t.display_name}”`);
    }
  }

  return (
    <Section
      id="trusted-issuers"
      title="Trusted issuers"
      description="An identity token from one of these is exchanged for a short-lived key that can deploy as the person it names."
      actions={
        <Button size="sm" variant="outline" onClick={() => setEditing("new")}>
          <Plus className="mr-1.5 h-4 w-4" />
          Add issuer
        </Button>
      }
    >
      {issuers.error ? (
        <div className="p-4">
          <QueryError
            title="The trusted issuers could not be read"
            error={issuers.error}
            fallback={ADMIN_ONLY_FALLBACK}
            onRetry={() => issuers.refetch()}
          />
        </div>
      ) : issuers.isLoading ? (
        <div className="space-y-3 p-4">
          <Skeleton className="h-4 w-48" />
          <Skeleton className="h-4 w-64" />
        </div>
      ) : (issuers.data ?? []).length === 0 ? (
        <EmptyState
          title="No issuer is trusted"
          description="Every deploy uses a personal or deploy key. Add an issuer to let Posit Workbench or a Garth session sign people in without one."
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead className="w-full">Issuer</TableHead>
              <TableHead className="hidden @2xl:table-cell">Names people by</TableHead>
              <TableHead className="hidden @xl:table-cell">Key lifetime</TableHead>
              <TableHead className="hidden @md:table-cell">Live keys</TableHead>
              <TableHead>On</TableHead>
              <TableHead><span className="sr-only">Actions</span></TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(issuers.data ?? []).map((t) => (
              <TableRow key={t.id}>
                <TableCell className="max-w-0">
                  <div className="truncate font-medium">{t.display_name}</div>
                  <div className="truncate font-mono text-xs text-muted-foreground">
                    {t.issuer} · {t.audience}
                  </div>
                </TableCell>
                <TableCell className="hidden text-sm @2xl:table-cell">
                  {t.subject_claim === "email" ? "Verified email" : `Identity on ${providerName(t.provider_id)}`}
                </TableCell>
                <TableCell className="hidden text-sm @xl:table-cell">{lifetimeLabel(t.key_lifetime_minutes)}</TableCell>
                <TableCell className="hidden text-sm tabular-nums @md:table-cell">{t.active_keys}</TableCell>
                <TableCell>
                  <Switch
                    checked={t.enabled}
                    onCheckedChange={(v) => toggle(t, v)}
                    aria-label={`${t.display_name} may vouch for people`}
                  />
                </TableCell>
                <TableCell>
                  <DropdownMenu>
                    <DropdownMenuTrigger
                      render={<Button variant="ghost" size="icon-sm" aria-label={`Actions for ${t.display_name}`} />}
                    >
                      <MoreHorizontal className="h-4 w-4" />
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end">
                      <DropdownMenuItem onClick={() => setEditing(t)}>Edit…</DropdownMenuItem>
                      <DropdownMenuItem variant="destructive" onClick={() => setRemoving(t)}>Remove…</DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      {(keys.data ?? []).length > 0 && (
        <div className="border-t border-border/60 p-4">
          <SectionHeading level={3} title="Live short-lived keys" />
          <Table className="mt-2">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="w-full">Person</TableHead>
                <TableHead className="hidden @xl:table-cell">Vouched for by</TableHead>
                <TableHead className="hidden @md:table-cell">Last used</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead><span className="sr-only">Actions</span></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(keys.data ?? []).map((k) => (
                <TableRow key={k.id}>
                  <TableCell className="max-w-0">
                    <div className="truncate font-medium">{k.user_login}</div>
                    <div className="truncate font-mono text-xs text-muted-foreground">{k.key_prefix}…</div>
                  </TableCell>
                  <TableCell className="hidden text-sm @xl:table-cell">{k.issuer_name}</TableCell>
                  <TableCell className="hidden text-sm @md:table-cell">
                    {k.last_used_at ? <RelativeTime iso={k.last_used_at} /> : "Never"}
                  </TableCell>
                  <TableCell className="text-sm"><RelativeTime iso={k.expires_at} /></TableCell>
                  <TableCell>
                    <Button variant="ghost" size="sm" onClick={() => setRevoking(k)}>Revoke…</Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {editing !== null && (
        <IssuerDialog
          key={editing === "new" ? "new" : editing.id}
          issuer={editing === "new" ? null : editing}
          providers={ssoProviders}
          onOpenChange={() => setEditing(null)}
          onSaved={refresh}
        />
      )}
      <RemoveIssuerDialog issuer={removing} onOpenChange={() => setRemoving(null)} onRemoved={refresh} />
      <RevokeKeyDialog exchangedKey={revoking} onOpenChange={() => setRevoking(null)} onRevoked={refresh} />
    </Section>
  );
}

function lifetimeLabel(minutes: number) {
  const hours = minutes / 60;
  return Number.isInteger(hours) ? `${hours} hour${hours === 1 ? "" : "s"}` : `${minutes} minutes`;
}

function IssuerDialog({
  issuer,
  providers,
  onOpenChange,
  onSaved,
}: {
  issuer: TrustedIssuer | null;
  providers: AuthProvider[];
  onOpenChange: (v: boolean) => void;
  onSaved: () => void;
}) {
  const [draft, setDraft] = useState<TrustedIssuerInput>({
    display_name: issuer?.display_name ?? "",
    issuer: issuer?.issuer ?? "",
    audience: issuer?.audience ?? "",
    subject_claim: issuer?.subject_claim ?? "sub",
    provider_id: issuer?.provider_id ?? providers[0]?.id ?? "",
    key_lifetime_minutes: issuer?.key_lifetime_minutes ?? 480,
  });
  const [hours, setHours] = useState(String((issuer?.key_lifetime_minutes ?? 480) / 60));
  const [saving, setSaving] = useState(false);
  const set = (patch: Partial<TrustedIssuerInput>) => setDraft((d) => ({ ...d, ...patch }));

  const hoursNum = Number(hours);
  const hoursError =
    hours.trim() === "" || !Number.isInteger(hoursNum) || hoursNum < 1 || hoursNum > 24
      ? "A whole number of hours, from 1 to 24"
      : "";
  const ready =
    draft.display_name.trim() !== "" && draft.issuer.trim() !== "" && draft.audience.trim() !== "" &&
    (draft.subject_claim === "email" || draft.provider_id !== "") && !hoursError;

  async function submit() {
    setSaving(true);
    const body = { ...draft, key_lifetime_minutes: hoursNum * 60 };
    try {
      if (issuer) await api.updateTrustedIssuer(issuer.id, body);
      else await api.createTrustedIssuer(body);
      toast.success(issuer ? "Saved" : `“${draft.display_name}” can vouch for people`);
      onOpenChange(false);
      onSaved();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t save “${draft.display_name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{issuer ? `Edit “${issuer.display_name}”` : "Add a trusted issuer"}</DialogTitle>
          <DialogDescription>
            A token is trusted only when it was issued by this issuer, for this audience, and has not expired.
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="ti-name">Name</Label>
            <Input id="ti-name" value={draft.display_name} placeholder="Posit Workbench"
              onChange={(e) => set({ display_name: e.target.value })} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ti-issuer">Issuer</Label>
            <Input id="ti-issuer" className="font-mono" value={draft.issuer} placeholder="https://login.example.com"
              onChange={(e) => set({ issuer: e.target.value })} />
            <FieldHelp>Exactly as the tokens’ <Code>iss</Code> claim carries it.</FieldHelp>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ti-aud">Audience</Label>
            <Input id="ti-aud" className="font-mono" value={draft.audience}
              onChange={(e) => set({ audience: e.target.value })} />
            <FieldHelp>The caller’s client ID at the issuer — what its tokens carry in <Code>aud</Code>.</FieldHelp>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ti-claim">Finds the person by</Label>
            <Select value={draft.subject_claim} onValueChange={(v) => v && set({ subject_claim: v as "sub" | "email" })}>
              <SelectTrigger id="ti-claim" className="w-full">
                <SelectValue>{(v) => (v === "email" ? "Verified email" : "Identity on a sign-in provider")}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="sub">Identity on a sign-in provider</SelectItem>
                <SelectItem value="email">Verified email</SelectItem>
              </SelectContent>
            </Select>
            <FieldHelp>Nobody is created: the person must have signed in to this install before.</FieldHelp>
          </div>
          {draft.subject_claim === "sub" && (
            <div className="space-y-1.5">
              <Label htmlFor="ti-provider">Sign-in provider</Label>
              <Select value={draft.provider_id} onValueChange={(v) => v && set({ provider_id: v })}>
                <SelectTrigger id="ti-provider" className="w-full">
                  <SelectValue>
                    {(v) => (v ? providers.find((p) => p.id === v)?.display_name ?? v : "Choose a provider…")}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {providers.map((p) => (
                    <SelectItem key={p.id} value={p.id}>{p.display_name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldHelp>The token’s <Code>sub</Code> is looked up among this provider’s accounts.</FieldHelp>
            </div>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="ti-hours">Key lifetime, in hours</Label>
            <Input id="ti-hours" type="number" min={1} max={24} className="w-28" value={hours}
              aria-invalid={!!hoursError} aria-describedby={hoursError ? fieldErrorId("ti-hours") : undefined}
              onChange={(e) => setHours(e.target.value)} />
            {hoursError ? <FieldError field="ti-hours">{hoursError}</FieldError> : (
              <FieldHelp>Each key ends on its own after this long. Revoking one ends it sooner.</FieldHelp>
            )}
          </div>
        </DialogBody>
        <DialogFooter>
          <Button variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button disabled={!ready || saving} onClick={submit}>
            {saving ? "Saving…" : issuer ? "Save" : "Add issuer"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function RemoveIssuerDialog({
  issuer,
  onOpenChange,
  onRemoved,
}: {
  issuer: TrustedIssuer | null;
  onOpenChange: (v: boolean) => void;
  onRemoved: () => void;
}) {
  const [busy, setBusy] = useState(false);
  async function submit() {
    if (!issuer) return;
    setBusy(true);
    try {
      await api.deleteTrustedIssuer(issuer.id);
      toast.success(`Removed “${issuer.display_name}”`);
      onOpenChange(false);
      onRemoved();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t remove “${issuer.display_name}”`);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Dialog open={issuer !== null} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Remove “{issuer?.display_name}”?</DialogTitle>
          <DialogDescription>
            {issuer?.active_keys
              ? `Its ${issuer.active_keys} live key${issuer.active_keys === 1 ? "" : "s"} stop working at once. `
              : ""}
            No account is changed. To stop it vouching for a while, switch it off instead.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="destructive" disabled={busy} onClick={submit}>
            {busy ? "Removing…" : "Remove issuer"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function RevokeKeyDialog({
  exchangedKey,
  onOpenChange,
  onRevoked,
}: {
  exchangedKey: ExchangedKey | null;
  onOpenChange: (v: boolean) => void;
  onRevoked: () => void;
}) {
  const [busy, setBusy] = useState(false);
  async function submit() {
    if (!exchangedKey) return;
    setBusy(true);
    try {
      await api.revokeExchangedKey(exchangedKey.id);
      toast.success(`Revoked ${exchangedKey.user_login}’s key`);
      onOpenChange(false);
      onRevoked();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t revoke ${exchangedKey.user_login}’s key`);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Dialog open={exchangedKey !== null} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Revoke {exchangedKey?.user_login}’s key?</DialogTitle>
          <DialogDescription>
            It stops working at once. {exchangedKey?.issuer_name} can exchange a fresh token for a new one while it
            still trusts the person.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="destructive" disabled={busy} onClick={submit}>
            {busy ? "Revoking…" : "Revoke key"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
