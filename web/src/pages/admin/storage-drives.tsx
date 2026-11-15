import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { MoreHorizontal } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import {
  Dialog, DialogBody, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { FieldHelp } from "@/components/field-help";
import { Section } from "@/components/section";
import { ApiError, api } from "@/lib/api";
import type { StorageDrive, StorageRepointCheck } from "@/lib/api";
import { toneSurface, toneText } from "@/lib/tone";
import { cn } from "@/lib/utils";

/**
 * The drives this install has — host roots and a cluster's declared classes —
 * and what an administrator calls each (ADR §12a).
 *
 * **The operator declares a drive; the administrator names it.** A key like
 * `efs` is infrastructure, and renaming it meant a Terraform change and a SQL
 * update at the same moment. A label is a row here, edited in place, and reaches
 * nothing but these pages: apps and the CLI use resource names, never drive
 * names.
 *
 * Below the resources rather than above them: the resources are what an
 * administrator acts on daily, and a drive is the shape of the install. A
 * stranded drive is the exception, and the page's notice links here.
 */
export function DrivesSection({
  drives, onChanged, onRepoint,
}: {
  drives: StorageDrive[];
  onChanged: () => void;
  /** Offered on a drive this install no longer declares, with resources still on it. */
  onRepoint?: (drive: StorageDrive) => void;
}) {
  const [editing, setEditing] = useState<StorageDrive | null>(null);
  if (drives.length === 0) return null;

  return (
    <Section
      id="drives"
      title="Drives"
      description="Where volumes can live. Whoever runs Toft declares them; you decide what they’re called."
    >
      <Table>
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead className="w-full">Drive</TableHead>
            <TableHead className="hidden @xl:table-cell">Location</TableHead>
            <TableHead className="text-right">Resources</TableHead>
            <TableHead className="w-[50px]"></TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {drives.map((d) => (
            <TableRow key={d.key}>
              <TableCell className="max-w-0 whitespace-normal">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className="font-medium">{d.label}</span>
                  {d.label !== d.name && (
                    <code className="font-mono text-xs text-muted-foreground">{d.name}</code>
                  )}
                  {!d.declared && (
                    <Badge variant="outline" className={cn("text-2xs", toneSurface.attention)}>
                      No longer declared
                    </Badge>
                  )}
                  {d.read_only && (
                    <Badge variant="outline" className="text-2xs">Read-only</Badge>
                  )}
                  {d.default && (
                    <Badge variant="outline" className="text-2xs">Default</Badge>
                  )}
                </div>
                {d.description && (
                  <div className="mt-0.5 text-xs text-muted-foreground">{d.description}</div>
                )}
                <div className="mt-0.5 font-mono text-xs text-muted-foreground @xl:hidden">
                  {d.path ?? d.class}
                </div>
              </TableCell>
              <TableCell className="hidden font-mono text-xs text-muted-foreground @xl:table-cell">
                {d.path ?? d.class ?? "—"}
              </TableCell>
              <TableCell className="text-right tabular-nums">{d.resource_count}</TableCell>
              <TableCell>
                <DropdownMenu>
                  <DropdownMenuTrigger
                    render={
                      <Button
                        variant="ghost"
                        size="icon"
                        className="h-7 w-7"
                        aria-label={`Actions for drive ${d.label}`}
                      >
                        <MoreHorizontal className="h-4 w-4" />
                      </Button>
                    }
                  />
                  <DropdownMenuContent align="end">
                    <DropdownMenuItem onClick={() => setEditing(d)}>Edit label…</DropdownMenuItem>
                    {onRepoint && !d.declared && d.resource_count > 0 && (
                      <DropdownMenuItem onClick={() => onRepoint(d)}>Re-point…</DropdownMenuItem>
                    )}
                  </DropdownMenuContent>
                </DropdownMenu>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>

      {editing && (
        <EditDriveLabelDialog
          key={editing.key}
          drive={editing}
          onOpenChange={() => setEditing(null)}
          onSaved={onChanged}
        />
      )}
    </Section>
  );
}

/**
 * What a drive is called. No confirmation: a label is read by these pages
 * alone, so changing one moves no file, no mapping and no mount, and nothing
 * restarts.
 */
function EditDriveLabelDialog({
  drive, onOpenChange, onSaved,
}: {
  drive: StorageDrive;
  onOpenChange: () => void;
  onSaved: () => void;
}) {
  const [label, setLabel] = useState(drive.label === drive.name ? "" : drive.label);
  const [description, setDescription] = useState(drive.description ?? "");
  const [saving, setSaving] = useState(false);

  async function submit() {
    setSaving(true);
    try {
      await api.updateStorageDriveLabel(drive.key, {
        label: label.trim(),
        description: description.trim(),
      });
      toast.success("Saved");
      onOpenChange();
      onSaved();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t save the label of “${drive.name}”`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open onOpenChange={(o) => { if (!saving && !o) onOpenChange(); }}>
      <DialogContent scrollable>
        <form
          className="contents"
          onSubmit={(e) => {
            e.preventDefault();
            if (!saving) void submit();
          }}
        >
        <DialogHeader>
          <DialogTitle>
            Label drive <span className="font-mono">{drive.name}</span>
          </DialogTitle>
          <DialogDescription>
            Only these pages show it. Apps and the CLI keep using resource names.
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="drive-label">Label</Label>
            <Input
              id="drive-label"
              value={label}
              maxLength={60}
              onChange={(e) => setLabel(e.target.value)}
              placeholder={drive.name}
            />
            <FieldHelp>Leave it empty to show the drive’s own name.</FieldHelp>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="drive-description">Description (optional)</Label>
            <Textarea
              id="drive-description"
              rows={3}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="What it’s for, and who looks after it"
            />
          </div>
        </DialogBody>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onOpenChange} disabled={saving}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving}>
            {saving ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Moving a stranded drive's resources onto the drive that now holds them.
 *
 * **The proof is the confirmation.** Choosing a target runs the server's proof
 * as a dry run, and what it found — every directory, or every file system —
 * is on screen before the button is. Confirm then proves again and moves all
 * or none. A plain confirm rather than type-to-confirm: it is undone by
 * pointing back while the old name can still be declared.
 */
export function RepointDriveDialog({
  drive, targets, onOpenChange, onDone,
}: {
  drive: StorageDrive;
  /** Declared drives of the same kind. */
  targets: StorageDrive[];
  onOpenChange: () => void;
  onDone: () => void;
}) {
  const [to, setTo] = useState(targets.length === 1 ? targets[0].key : "");
  const [saving, setSaving] = useState(false);

  // The proof, as a dry run keyed on the chosen drive: what it found is on
  // screen before the button is enabled, and never cached past this dialog.
  const proofQuery = useQuery({
    queryKey: ["storage-repoint-proof", drive.key, to],
    queryFn: () => api.repointStorageDrive(drive.key, { to, dry_run: true }),
    enabled: !!to,
    retry: false,
    gcTime: 0,
  });
  const refusal = proofQuery.error;
  const checks: StorageRepointCheck[] =
    proofQuery.data?.checks ??
    ((refusal instanceof ApiError ? refusal.body.checks : undefined) as StorageRepointCheck[] | undefined) ??
    [];
  const proof = to
    ? {
        key: to,
        checks,
        error: refusal ? (refusal instanceof Error ? refusal.message : "Couldn’t check that drive") : undefined,
      }
    : null;
  const checking = proofQuery.isFetching;

  async function submit() {
    setSaving(true);
    try {
      const answer = await api.repointStorageDrive(drive.key, { to });
      const target = targets.find((t) => t.key === to);
      const n = answer.checks.length;
      toast.success(`Moved ${n} ${n === 1 ? "resource" : "resources"} to “${target?.label ?? to}”`, {
        description: "Each app picks up the new path at its next start.",
      });
      onOpenChange();
      onDone();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : `Couldn’t re-point “${drive.label}”`);
    } finally {
      setSaving(false);
    }
  }

  const proven = !!proofQuery.data && !refusal && checks.length > 0;

  return (
    <Dialog open onOpenChange={(o) => { if (!saving && !o) onOpenChange(); }}>
      <DialogContent scrollable className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>
            Re-point <span className="font-mono">{drive.name}</span>
          </DialogTitle>
          <DialogDescription>
            Moves its {drive.resource_count} resource{drive.resource_count === 1 ? "" : "s"} to the
            drive that now holds their files. No files are copied.
          </DialogDescription>
        </DialogHeader>
        <DialogBody className="space-y-4">
          {targets.length === 0 ? (
            <FieldHelp>
              No declared drive to move them to. Whoever runs Toft has to declare the drive
              that holds these files first.
            </FieldHelp>
          ) : (
            <div className="space-y-1.5">
              <Label htmlFor="repoint-to">Move to</Label>
              <Select value={to} onValueChange={(v) => setTo(v ?? "")}>
                <SelectTrigger id="repoint-to" className="w-full">
                  <SelectValue>
                    {(v) => {
                      const t = targets.find((x) => x.key === v);
                      return t ? (t.label === t.name ? t.name : `${t.label} (${t.name})`) : "Choose a drive…";
                    }}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent>
                  {targets.map((t) => (
                    <SelectItem key={t.key} value={t.key}>
                      {t.label === t.name ? t.name : `${t.label} (${t.name})`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}

          {checking && <FieldHelp>Checking each resource…</FieldHelp>}
          {proof?.key === to && proof.checks.length > 0 && (
            <ul className="space-y-1 text-xs">
              {proof.checks.map((c) => (
                <li key={c.slug} className="flex flex-wrap gap-x-2">
                  <span className="font-mono font-medium">{c.slug}</span>
                  {c.problem ? (
                    <span className={toneText.danger}>{c.problem}</span>
                  ) : (
                    <span className="font-mono text-muted-foreground">{c.evidence}</span>
                  )}
                </li>
              ))}
            </ul>
          )}
          {proof?.key === to && proof.error && (
            <FieldHelp tone="danger">{proof.error}</FieldHelp>
          )}
        </DialogBody>
        <DialogFooter>
          <Button variant="outline" onClick={onOpenChange} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={!proven || saving}>
            {saving ? "Moving…" : "Re-point"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
