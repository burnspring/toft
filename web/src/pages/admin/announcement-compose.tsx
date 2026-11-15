import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Copy, RotateCw, Send, TestTube2, Trash2 } from "lucide-react";

import { api } from "@/lib/api";
import type { AnnouncementAudience, AnnouncementEmail } from "@/lib/api";
import { useDocumentTitle } from "@/hooks/use-document-title";
import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { FieldLabel, Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
import { Checkbox } from "@/components/ui/checkbox";
import { Skeleton } from "@/components/ui/skeleton";
import { Section } from "@/components/section";
import { PageHeader } from "@/components/page-header";
import { ParentCrumb } from "@/components/parent-crumb";
import { EmptyState } from "@/components/empty-state";
import { FieldError } from "@/components/field-help";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { useUnloadGuard } from "@/hooks/use-unload-guard";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";
import { cn, fieldErrorId } from "@/lib/utils";
import { TableCard } from "@/components/table-card";
import { Notice } from "@/components/notice";
import { QueryError } from "@/components/query-error";
import { RelativeTime } from "@/components/relative-time";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { AnnouncementCounts } from "@/components/announcement-email-list";
import { DeleteAnnouncementDialog } from "@/components/delete-announcement-dialog";
import { FieldHelp } from "@/components/field-help";
import { AUDIENCE_LABEL } from "@/lib/announcements";
import { toneSurface, toneText, type Tone } from "@/lib/tone";

/**
 * Admin → Messages → an announcement.
 *
 * Two pages in one component, and which one you get is the row's status. A
 * draft is a compose box; a frozen row is a record, and the record is what the
 * whole feature is for — so it leads with who did not get it rather than with
 * what was said.
 *
 * **The confirmation is the estimate.** There is no un-send, so the dialog's job
 * is to put the number in front of somebody *before* the freeze — including the
 * case where the number is zero deliverable, which is otherwise something an
 * administrator discovers afterwards from a row reading `0 delivered`. It is a
 * plain confirm rather than type-to-confirm: what is irreversible here is one
 * message to a named audience, and asking somebody to retype a subject line
 * they wrote thirty seconds ago is an obstacle rather than a check.
 */

const MAX_SUBJECT = 200;
const MAX_BODY = 4000;
const MAX_LINK_TEXT = 60;
/** The banner's own cap, `announcementMaxLen` in announcement_handlers.go. */
const MAX_BANNER = 500;

/**
 * What happened to one person, as a word and a colour.
 *
 * `skipped` is `neutral` rather than `attention`: nothing malfunctioned — the
 * account was locked or has no address, and both are the install being what it
 * is. `abandoned` is `danger` with `delivered` because from the reader's side
 * they are the same fact, which is that this person was not told.
 */
const RECIPIENT: Record<string, { label: string; tone: Tone }> = {
  delivered: { label: "Delivered", tone: "ok" },
  failed: { label: "Failed", tone: "danger" },
  abandoned: { label: "Abandoned", tone: "danger" },
  skipped: { label: "Skipped", tone: "neutral" },
  pending: { label: "Sending", tone: "info" },
};

/** Why somebody was not mailed, in words rather than in the stored token. */
const SKIP_REASON: Record<string, string> = {
  locked: "Account is locked",
  no_address: "No email address on file",
};

/** What `/admin/messages/announcement/new` edits: nothing on the server yet.
 *  The row is created by the first save, so backing out leaves no junk draft. */
function blankDraft(canSend: boolean): AnnouncementEmail {
  const now = new Date().toISOString();
  return {
    id: "",
    subject: "",
    body: "",
    audience: "all",
    status: "draft",
    created_at: now,
    updated_at: now,
    resolved: 0,
    delivered: 0,
    failed: 0,
    skipped: 0,
    abandoned: 0,
    can_send: canSend,
  };
}

export function AdminAnnouncementComposePage() {
  const { id = "" } = useParams();
  const isNew = id === "new";
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: ["announcement-emails", id],
    queryFn: () => api.getAnnouncementEmail(id),
    enabled: !isNew,
    // While a run is in flight the counts move, and an administrator watching
    // this page is watching exactly that. It stops the moment the row is sent.
    refetchInterval: (q) => (q.state.data?.status === "sending" ? 2000 : false),
  });
  // A new draft only needs to know whether this install can send at all.
  const list = useQuery({
    queryKey: ["announcement-emails"],
    queryFn: api.listAnnouncementEmails,
    enabled: isNew,
  });
  const announcement = isNew
    ? list.data
      ? blankDraft(list.data.can_send)
      : undefined
    : query.data;
  const failed = isNew ? list : query;

  useDocumentTitle(announcement?.subject || "New announcement");

  if (failed.isError) {
    return (
      <div className="space-y-4">
        <ParentCrumb to={MESSAGES} parent="Messages" />
        <QueryError
          error={failed.error}
          title="This announcement could not be read"
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void failed.refetch()}
        />
      </div>
    );
  }
  if (!announcement) {
    return (
      <div className="space-y-4">
        <ParentCrumb to={MESSAGES} parent="Messages" />
        <Skeleton className="h-64 w-full" />
      </div>
    );
  }

  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: ["announcement-emails"] });
  };

  return (
    <div className="space-y-6">
      {announcement.status === "draft" ? (
        <ComposeDraft
          announcement={announcement}
          onChanged={refresh}
          onCreated={(created) => {
            // Seeded so the page does not flash a skeleton between the URL
            // changing and the read answering; the form keeps its state.
            queryClient.setQueryData(["announcement-emails", created.id], created);
            navigate(`/admin/messages/announcement/${created.id}`, { replace: true });
          }}
          onDeleted={() => {
            refresh();
            navigate(MESSAGES);
          }}
        />
      ) : (
        <SentRecord
          announcement={announcement}
          onChanged={refresh}
          onDeleted={() => {
            refresh();
            navigate(MESSAGES);
          }}
        />
      )}
    </div>
  );
}

const MESSAGES = "/admin/messages#announcement-emails";

/* ------------------------------------------------------------------ *
 * The draft
 * ------------------------------------------------------------------ */

function ComposeDraft({
  announcement,
  onChanged,
  onCreated,
  onDeleted,
}: {
  announcement: AnnouncementEmail;
  onChanged: () => void;
  onCreated: (created: AnnouncementEmail) => void;
  onDeleted: () => void;
}) {
  const navigate = useNavigate();
  const [subject, setSubject] = useState(announcement.subject);
  const [body, setBody] = useState(announcement.body);
  const [linkUrl, setLinkUrl] = useState(announcement.link_url ?? "");
  const [linkText, setLinkText] = useState(announcement.link_text ?? "");
  const [audience, setAudience] = useState<AnnouncementAudience>(announcement.audience);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [leaving, setLeaving] = useState(false);
  const isNew = announcement.id === "";

  const estimate = useQuery({
    queryKey: ["announcement-estimate", audience],
    queryFn: () => api.estimateAnnouncementAudience(audience),
  });

  const dirty =
    subject !== announcement.subject ||
    body !== announcement.body ||
    linkUrl !== (announcement.link_url ?? "") ||
    linkText !== (announcement.link_text ?? "") ||
    audience !== announcement.audience;
  // Over a limit is flagged rather than cut: a paste used to lose its tail.
  const tooLong =
    subject.length > MAX_SUBJECT || body.length > MAX_BODY || linkText.length > MAX_LINK_TEXT;
  // A reload, a close or a typed URL; the crumb below asks for itself.
  useUnloadGuard(dirty, "The announcement has unsaved changes");

  /** Saves what is on screen and answers the row's id, or null on failure. A
   *  new draft is created here, on its first save, and nowhere earlier. */
  const save = async (): Promise<string | null> => {
    setSaving(true);
    const input = { subject, body, link_url: linkUrl, link_text: linkText, audience };
    try {
      if (isNew) {
        const created = await api.createAnnouncementEmail(input);
        onChanged();
        toast.success("Saved");
        onCreated(created);
        return created.id;
      }
      await api.updateAnnouncementEmail(announcement.id, input);
      onChanged();
      toast.success("Saved");
      return announcement.id;
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn’t save the announcement");
      return null;
    } finally {
      setSaving(false);
    }
  };

  const sendTest = async () => {
    // Saved first, because a test of what is on the screen is the only test
    // worth having — a preview of the stored version is a preview of something
    // the administrator is not looking at.
    const rowId = dirty || isNew ? await save() : announcement.id;
    if (!rowId) return;
    setTesting(true);
    try {
      const { sent_to } = await api.testAnnouncementEmail(rowId);
      toast.success(`A copy is on its way to ${sent_to}`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "Couldn’t send the test");
    } finally {
      setTesting(false);
    }
  };

  return (
    <>
      {/* The crumb asks before it throws edits away: a link inside the SPA is
          not an unload, so the guard above cannot see it. */}
      <div
        onClickCapture={(e) => {
          if (dirty) {
            e.preventDefault();
            setLeaving(true);
          }
        }}
      >
        <ParentCrumb to={MESSAGES} parent="Messages" current={subject || "New announcement"} />
      </div>

      <PageHeader
        title={subject || "New announcement"}
        description="Plain text with one optional link. Sent once; there is no un-send."
        actions={
          isNew ? undefined : (
            <Button variant="outline" size="sm" onClick={() => setDeleting(true)}>
              <Trash2 className="h-4 w-4" aria-hidden />
              Delete draft…
            </Button>
          )
        }
      />

      {!isNew && (
        <DeleteAnnouncementDialog
          announcement={announcement}
          open={deleting}
          onOpenChange={setDeleting}
          onDeleted={onDeleted}
        />
      )}

      <Dialog open={leaving} onOpenChange={setLeaving}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Leave without saving?</DialogTitle>
            <DialogDescription>
              Your edits to this announcement haven’t been saved.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setLeaving(false)}>
              Keep editing
            </Button>
            <Button variant="destructive" onClick={() => navigate(MESSAGES)}>
              Discard and leave
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Section title="Message">
        <div className="space-y-4 p-4">
          <div className="space-y-2">
            <Label htmlFor="announcement-subject">Subject</Label>
            <Input
              id="announcement-subject"
              value={subject}
              aria-invalid={subject.length > MAX_SUBJECT}
              onChange={(e) => setSubject(e.target.value)}
              placeholder="Postgres upgrade on Saturday"
            />
            <Counter id="announcement-subject" length={subject.length} max={MAX_SUBJECT} />
          </div>

          <div className="space-y-2">
            <Label htmlFor="announcement-body">Message</Label>
            <Textarea
              id="announcement-body"
              value={body}
              aria-invalid={body.length > MAX_BODY}
              onChange={(e) => setBody(e.target.value)}
              rows={10}
              placeholder="Expect an hour of downtime from 09:00. Nothing needs doing on your side."
            />
            <Counter id="announcement-body" length={body.length} max={MAX_BODY} />
            <FieldHelp>
              Plain text. Line breaks are kept; Markdown and HTML are not.
            </FieldHelp>
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="announcement-link">Link (optional)</Label>
              <Input
                id="announcement-link"
                value={linkUrl}
                onChange={(e) => setLinkUrl(e.target.value)}
                placeholder="https://…"
                className="font-mono text-xs"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="announcement-link-text">Link text</Label>
              <Input
                id="announcement-link-text"
                value={linkText}
                aria-invalid={linkText.length > MAX_LINK_TEXT}
                onChange={(e) => setLinkText(e.target.value)}
                placeholder="Read the plan"
                disabled={linkUrl.trim() === ""}
              />
              {linkText.length > MAX_LINK_TEXT && (
                <Counter id="announcement-link-text" length={linkText.length} max={MAX_LINK_TEXT} />
              )}
            </div>
          </div>
        </div>
      </Section>

      <Section
        title="Who it goes to"
        description="Worked out when you send, not now."
      >
        <div className="space-y-4 p-4">
          <div className="space-y-2">
            <FieldLabel>Audience</FieldLabel>
            <div role="group" aria-label="Audience" className="flex flex-wrap gap-2">
              {(["all", "publishers"] as AnnouncementAudience[]).map((a) => (
                <Button
                  key={a}
                  type="button"
                  size="sm"
                  variant={audience === a ? "default" : "outline"}
                  aria-pressed={audience === a}
                  onClick={() => setAudience(a)}
                >
                  {AUDIENCE_LABEL[a]}
                </Button>
              ))}
            </div>
          </div>

          {estimate.data && <EstimateLine estimate={estimate.data} />}
        </div>
      </Section>

      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="outline"
          onClick={() => void save()}
          disabled={saving || !dirty || tooLong}
        >
          {saving ? "Saving…" : "Save draft"}
        </Button>
        <Button
          variant="secondary"
          onClick={sendTest}
          disabled={testing || saving || tooLong || (isNew && !dirty)}
        >
          <TestTube2 className="h-4 w-4" aria-hidden />
          {testing ? "Sending…" : "Send a test to me"}
        </Button>
        {announcement.can_send && (
          <Button
            className="ml-auto"
            onClick={async () => {
              if ((dirty || isNew) && !(await save())) return;
              setConfirming(true);
            }}
            disabled={saving || tooLong || (isNew && !dirty)}
          >
            <Send className="h-4 w-4" aria-hidden />
            Send…
          </Button>
        )}
      </div>

      <SendDialog
        // Keyed on the subject so the banner's prefill follows an edit: the
        // dialog is always mounted and seeded its state once, so a draft whose
        // subject was changed published the old text as the banner (code
        // review 2026-11-11, N-2).
        key={subject}
        open={confirming}
        onOpenChange={setConfirming}
        announcement={{ ...announcement, subject, audience }}
        deliverable={estimate.data?.deliverable ?? 0}
        onSent={onChanged}
      />
    </>
  );
}

function Counter({ id, length, max }: { id: string; length: number; max: number }) {
  const over = length > max;
  return (
    <>
      <div
        className={cn("flex justify-end text-2xs", over ? toneText.danger : "text-muted-foreground")}
      >
        {length}/{max}
      </div>
      <FieldError field={id}>{over ? `Shorten this to ${max} characters.` : null}</FieldError>
    </>
  );
}

/**
 * The live count, with the word "about" on it and the skips named.
 *
 * The skips are the reason this line exists rather than a single number. The
 * biggest group on a real install is people with no email address, which the
 * directory does not show and which this is the only place anybody would ever
 * find out about.
 */
function EstimateLine({
  estimate,
}: {
  estimate: { total: number; deliverable: number; skipped_locked: number; skipped_no_address: number; resolved_at: string };
}) {
  const skipped = estimate.skipped_locked + estimate.skipped_no_address;
  return (
    <TableCard flat className="p-3 text-xs">
      <p className="text-foreground">
        About <strong>{estimate.deliverable}</strong>{" "}
        {estimate.deliverable === 1 ? "person" : "people"} would be mailed, as of{" "}
        <RelativeTime iso={estimate.resolved_at} />.
      </p>
      {skipped > 0 && (
        <p className="mt-1.5 text-muted-foreground">
          {estimate.skipped_no_address > 0 && (
            <>
              {estimate.skipped_no_address} of the {estimate.total} in this audience{" "}
              {estimate.skipped_no_address === 1 ? "has" : "have"} no email address on
              file and cannot be reached.{" "}
            </>
          )}
          {estimate.skipped_locked > 0 && (
            <>
              {estimate.skipped_locked}{" "}
              {estimate.skipped_locked === 1 ? "account is" : "accounts are"} locked.
            </>
          )}
        </p>
      )}
      {estimate.deliverable === 0 && (
        <p className="mt-1.5 text-muted-foreground">
          Nobody in this audience can be reached.
        </p>
      )}
    </TableCard>
  );
}

function SendDialog({
  open,
  onOpenChange,
  announcement,
  deliverable,
  onSent,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  announcement: AnnouncementEmail;
  deliverable: number;
  onSent: () => void;
}) {
  const queryClient = useQueryClient();
  const [sending, setSending] = useState(false);
  // The banner rides along on the client: the send is the act that cannot be
  // taken back, the banner is the one a key may publish and DELETE removes, and
  // both already write their own audit row. Off by default — most mail is not
  // also a line across every page.
  const [withBanner, setWithBanner] = useState(false);
  const [banner, setBanner] = useState(announcement.subject);
  const current = useQuery({
    queryKey: ["announcement"],
    queryFn: api.getAnnouncement,
    enabled: open,
    retry: false,
  });
  const bannerProblem =
    withBanner && banner.trim() === ""
      ? "The banner needs some text."
      : banner.length > MAX_BANNER
        ? `Shorten this to ${MAX_BANNER} characters.`
        : null;
  return (
    <Dialog open={open} onOpenChange={(o) => { if (!sending) onOpenChange(o); }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Send this announcement?</DialogTitle>
          <DialogDescription>
            About {deliverable} {deliverable === 1 ? "person" : "people"} in{" "}
            {AUDIENCE_LABEL[announcement.audience].toLowerCase()} will receive an
            email with the subject “{announcement.subject}”.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-2">
          <label className="flex items-center gap-2 text-sm text-foreground">
            <Checkbox
              checked={withBanner}
              disabled={sending}
              onCheckedChange={(on) => setWithBanner(on === true)}
            />
            Also show a banner on every page
          </label>
          {withBanner && (
            <div className="space-y-1">
              <Label htmlFor="announcement-banner-text" className="sr-only">
                Banner text
              </Label>
              <Input
                id="announcement-banner-text"
                value={banner}
                disabled={sending}
                aria-invalid={bannerProblem !== null}
                aria-describedby={bannerProblem ? fieldErrorId("announcement-banner-text") : undefined}
                onChange={(e) => setBanner(e.target.value)}
              />
              <FieldError field="announcement-banner-text">{bannerProblem}</FieldError>
              <FieldHelp>
                {current.data?.message
                  ? `Replaces the current banner, “${current.data.message}”. `
                  : ""}
                It stays up until you clear it on Messages.
              </FieldHelp>
            </div>
          )}
        </div>
        <Notice tone="attention" title="There is no un-send">
          Once it starts, the message can’t be changed or stopped.
        </Notice>
        <DialogFooter>
          <Button variant="outline" disabled={sending} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            disabled={sending || deliverable === 0 || bannerProblem !== null}
            onClick={async () => {
              setSending(true);
              try {
                await api.sendAnnouncementEmail(announcement.id);
              } catch (e) {
                toast.error(e instanceof Error ? e.message : "Couldn’t send the announcement");
                setSending(false);
                return;
              }
              onOpenChange(false);
              onSent();
              toast.success("Sending — the record updates as it goes");
              // Only after the send was accepted: a banner announcing mail
              // that never went out is the one combination worth preventing.
              if (withBanner) {
                try {
                  await api.setAnnouncement({ message: banner.trim(), level: "info" });
                  toast.success("The banner is up");
                } catch (e) {
                  toast.error(
                    `Couldn’t set the banner: ${e instanceof Error ? e.message : "unknown error"}`,
                  );
                } finally {
                  queryClient.invalidateQueries({ queryKey: ["announcement"] });
                }
              }
              setSending(false);
            }}
          >
            {sending ? "Sending…" : "Send now"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/* ------------------------------------------------------------------ *
 * The record
 * ------------------------------------------------------------------ */

function SentRecord({
  announcement,
  onChanged,
  onDeleted,
}: {
  announcement: AnnouncementEmail;
  onChanged: () => void;
  onDeleted: () => void;
}) {
  const navigate = useNavigate();
  const [cloning, setCloning] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [resending, setResending] = useState(false);
  const recipients = announcement.recipients ?? [];
  const missed = announcement.failed + announcement.abandoned;
  // The server re-counts and refuses with its own words; this decides only
  // whether the offer is on screen.
  const canResend =
    announcement.status === "sent" &&
    missed > 0 &&
    !announcement.resent_as &&
    announcement.can_send === true;
  const notReached = recipients.filter((r) => r.status !== "delivered");

  return (
    <>
      <ParentCrumb to={MESSAGES} parent="Messages" current={announcement.subject} />
      <PageHeader
        title={announcement.subject}
        description={
          announcement.sent_at ? (
            announcement.resend_of ? (
              <>
                Resent <RelativeTime iso={announcement.sent_at} />
                {announcement.sent_by_login && <> by {announcement.sent_by_login}</>} to the
                people{" "}
                <Link
                  to={`/admin/messages/announcement/${announcement.resend_of}`}
                  className="text-primary hover:underline"
                >
                  the original
                </Link>{" "}
                didn’t reach.
              </>
            ) : (
              <>
                Sent <RelativeTime iso={announcement.sent_at} />
                {announcement.sent_by_login && <> by {announcement.sent_by_login}</>} to{" "}
                {AUDIENCE_LABEL[announcement.audience].toLowerCase()}.
              </>
            )
          ) : undefined
        }
        actions={
          <div className="flex items-center gap-2">
            {/* Not "Send again": it sends nothing. It copies this into a new
                draft, and the label beside a notice about mailing everybody
                twice made the safe act look like the dangerous one. */}
            <Button
              variant="outline"
              size="sm"
              disabled={cloning}
              onClick={async () => {
                setCloning(true);
                try {
                  const clone = await api.cloneAnnouncementEmail(announcement.id);
                  onChanged();
                  navigate(`/admin/messages/announcement/${clone.id}`);
                  toast.success("Copied into a new draft");
                } catch (e) {
                  toast.error(e instanceof Error ? e.message : "Couldn’t copy this announcement");
                } finally {
                  setCloning(false);
                }
              }}
            >
              <Copy className="h-4 w-4" aria-hidden />
              {cloning ? "Copying…" : "Copy to a new draft"}
            </Button>
            {/* Absent while the run is in flight rather than disabled: the row
                is not refusing on principle, it is busy, and the button is back
                on its own when the last recipient completes. */}
            {announcement.status === "sent" && (
              <Button variant="outline" size="sm" onClick={() => setDeleting(true)}>
                <Trash2 className="h-4 w-4" aria-hidden />
                Delete…
              </Button>
            )}
          </div>
        }
      />

      <DeleteAnnouncementDialog
        announcement={announcement}
        open={deleting}
        onOpenChange={setDeleting}
        onDeleted={onDeleted}
      />

      <ResendDialog
        open={resending}
        onOpenChange={setResending}
        announcement={announcement}
        missed={missed}
        onResent={(resend) => {
          onChanged();
          navigate(`/admin/messages/announcement/${resend.id}`);
        }}
      />

      <Section title="What happened">
        <div className="space-y-3 p-4">
          <AnnouncementCounts announcement={announcement} />
          {announcement.status === "sending" && (
            <Notice tone="info" title="This is going out now" live>
              The counts update as it goes. If this install restarts first, the rest
              is recorded as abandoned, and you can resend to them afterwards.
            </Notice>
          )}
          {announcement.status === "sent" && announcement.resent_as ? (
            <Notice
              tone="info"
              title="Resent to the people this didn’t reach"
              action={
                <Link
                  to={`/admin/messages/announcement/${announcement.resent_as}`}
                  className={buttonVariants({ variant: "outline", size: "sm" })}
                >
                  Open the resend
                </Link>
              }
            >
              Anybody the resend missed in turn is resent from there.
            </Notice>
          ) : (
            announcement.status === "sent" &&
            missed > 0 && (
              <Notice
                tone="danger"
                title={`${missed} didn’t get it`}
                action={
                  canResend ? (
                    <Button variant="outline" size="sm" onClick={() => setResending(true)}>
                      <RotateCw className="h-4 w-4" aria-hidden />
                      Resend to them…
                    </Button>
                  ) : undefined
                }
              >
                {announcement.failed > 0 && "A failed send was tried four times. "}
                {announcement.abandoned > 0 && "An abandoned one was cut off by a restart. "}
                A resend goes to these people only.
              </Notice>
            )
          )}
        </div>
      </Section>

      <Section title="Message" description="Frozen at the moment it was sent.">
        <div className="space-y-3 p-4 text-sm">
          <p className="whitespace-pre-wrap text-foreground">{announcement.body}</p>
          {announcement.link_url && (
            <p className="text-xs">
              <span className="text-muted-foreground">Link: </span>
              <span className="font-mono">{announcement.link_url}</span>
              {announcement.link_text && (
                <span className="text-muted-foreground"> — “{announcement.link_text}”</span>
              )}
            </p>
          )}
        </div>
      </Section>

      <Section
        title="Recipients"
        description={
          recipients.length === 0
            ? "The per-person list has been pruned. The counts above are kept."
            : "Who didn’t get it comes first."
        }
      >
        {recipients.length === 0 ? (
          <EmptyState
            size="inline"
            title="Nothing to show"
            description="How long the per-person list is kept is set on Admin → Settings."
            action={
              <Link
                to="/admin/settings#retention"
                className={buttonVariants({ variant: "outline", size: "sm" })}
              >
                Retention setting
              </Link>
            }
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Person</TableHead>
                <TableHead className="hidden @md:table-cell">Address</TableHead>
                <TableHead>Outcome</TableHead>
                <TableHead>Why</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {[...notReached, ...recipients.filter((r) => r.status === "delivered")].map((r) => (
                <TableRow key={r.id}>
                  <TableCell className="font-medium text-foreground">{r.login}</TableCell>
                  <TableCell className="hidden font-mono text-xs text-muted-foreground @md:table-cell">
                    {r.email || "—"}
                  </TableCell>
                  <TableCell>
                    <Badge
                      variant="outline"
                      className={toneSurface[RECIPIENT[r.status]?.tone ?? "neutral"]}
                    >
                      {RECIPIENT[r.status]?.label ?? r.status}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {r.reason ? (SKIP_REASON[r.reason] ?? r.reason) : "—"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>
    </>
  );
}

function ResendDialog({
  open,
  onOpenChange,
  announcement,
  missed,
  onResent,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  announcement: AnnouncementEmail;
  missed: number;
  onResent: (resend: AnnouncementEmail) => void;
}) {
  const [sending, setSending] = useState(false);
  return (
    <Dialog open={open} onOpenChange={(o) => { if (!sending) onOpenChange(o); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Resend to the people it didn’t reach?</DialogTitle>
          <DialogDescription>
            “{announcement.subject}” goes again, unchanged, to the {missed}{" "}
            {missed === 1 ? "person" : "people"} whose send failed or was abandoned — at
            the address on file now. Accounts locked or deleted since are left out.
          </DialogDescription>
        </DialogHeader>
        <Notice tone="attention" title="There is no un-send">
          It is recorded as a new announcement, and this record stays as it is.
        </Notice>
        <DialogFooter>
          <Button variant="outline" disabled={sending} onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            disabled={sending}
            onClick={async () => {
              setSending(true);
              try {
                const resend = await api.resendAnnouncementEmail(announcement.id);
                onOpenChange(false);
                toast.success("Resending — the record updates as it goes");
                onResent(resend);
              } catch (e) {
                toast.error(e instanceof Error ? e.message : "Couldn’t resend the announcement");
              } finally {
                setSending(false);
              }
            }}
          >
            {sending ? "Resending…" : "Resend now"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
