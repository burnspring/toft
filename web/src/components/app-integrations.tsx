import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Send } from "lucide-react";
import { toast } from "sonner";

import { Code } from "@/components/code";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Separator } from "@/components/ui/separator";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  Label,
} from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import { DestinationSelect } from "@/components/destination-select";
import { EmptyState } from "@/components/empty-state";
import { QueryError } from "@/components/query-error";
import { SendStatusBadge } from "@/components/send-status";
import { StatusBadge } from "@/components/status-badge";
import {
  api,
  type AppIntegrationRow,
  type AppIntegrations,
  type IntegrationSend,
} from "@/lib/api";
import { Notice } from "@/components/notice";
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Section, SectionHeading } from "@/components/section";
import { TableCard } from "@/components/table-card";

/**
 * What this app may send, and where it went.
 *
 * The shape of this tab follows the one rule the feature turns on: **attaching
 * is an administrator's decision, configuring what is attached belongs to the
 * app's people.** An owner who could enable their own app's email could mail
 * the company from an address the company trusts — so the switches that grant
 * a capability are admin-only and the ones that shape it are not.
 *
 * Not attached is a *sentence*, not a disabled control. An app with no
 * integration has none — absent, not off — and telling its owner who to ask is
 * more use than a greyed-out toggle they will click anyway.
 *
 * **One row per kind, and the kinds come from the server.** This used to be
 * two hand-written cards; a third would have been a third. What varies between
 * them is data now — whether there is a destination to choose, whether it can
 * be picked from a menu, and whether the kind leaves anything behind — so a
 * ninth integration is a row in `appint.Known` and nothing here.
 *
 * **One switch per kind.** There is no separate grant for uploading a file or
 * attaching one: an app trusted to post to a channel is trusted to put a file
 * in it. See appint's capability constants for the arithmetic that killed the
 * second switch.
 */
export function AppIntegrationsTab({
  appId,
  isAdmin,
  isStatic,
}: {
  appId: string;
  isAdmin: boolean;
  /** A static app runs no process and no job, so it has nothing to send from. */
  isStatic: boolean;
}) {
  // The page mounts this only for an editor: a tab the caller doesn't have is
  // absent and redirects, never rendered read-only.
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["app-integrations", appId],
    queryFn: () => api.getAppIntegrations(appId),
  });

  // Before the skeleton, or a failed read leaves this tab loading for ever.
  if (error) {
    return (
      <div className="pt-4">
        <QueryError
          title="This app’s integrations could not be read"
          error={error}
          fallback="Only the app’s owner and editors can see its integrations."
          onRetry={() => void refetch()}
        />
      </div>
    );
  }
  if (isLoading || !data) {
    return (
      <div className="space-y-4 pt-4">
        <Skeleton className="h-40 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  return (
    <div className="space-y-8 pt-4">
      {data.integrations.length === 0 ? (
        <>
          <SectionHeading
            title="What this app can send"
            description="Toft holds the credentials and sends on the app’s behalf. Every send is logged below."
          />
          {isStatic ? (
            <p className="max-w-prose text-sm text-muted-foreground">
              A static site runs no process and no job, so it has nothing to send from. Deploy it
              as an app in a language Toft runs to attach an integration.
            </p>
          ) : (
            <NothingConnected isAdmin={isAdmin} />
          )}
        </>
      ) : (
        <Section
          title="What this app can send"
          description="Toft holds the credentials and sends on the app’s behalf. Every send is logged below."
          className="divide-y divide-border/60"
        >
          {data.integrations.map((row) => (
            <IntegrationCard key={row.kind} appId={appId} row={row} data={data} isAdmin={isAdmin} />
          ))}
        </Section>
      )}

      <Separator />
      <RecentSends
        appId={appId}
        titles={Object.fromEntries(data.integrations.map((i) => [i.kind, i.title]))}
      />
    </div>
  );
}

/**
 * Nothing is connected on this install at all, said in words rather than shown
 * as a page of dead controls.
 *
 * An administrator reading this gets the way to fix it. The page used to say
 * "This install does not offer Slack to apps." to everybody, with no route
 * anywhere — and the person most likely to be reading it is the one who could
 * connect it, one click away on a page that sentence did not name.
 */
function NothingConnected({ isAdmin }: { isAdmin: boolean }) {
  return (
    <p className="text-sm text-muted-foreground">
      This install has nothing connected for apps to send through.{" "}
      {isAdmin && (
        <Link to="/admin/integrations" className="underline underline-offset-2 hover:text-foreground">
          Connect one in Admin → Integrations
        </Link>
      )}
    </p>
  );
}

/**
 * The words above the destination picker, which differ by what the kind
 * actually does with it.
 *
 * A file kind's destination is where files land and nothing else; a chat kind's
 * carries the platform's own notices too, but only where the kind is one the
 * platform posts through. Saying "Toft's notices about it" over a folder
 * that will never receive one is the kind of copy that teaches people not to
 * read it.
 */
function destinationHeading(row: AppIntegrationRow): string {
  if (row.kind === "email") return "Replies go to";
  if (row.leaves_residue) return "Files go to";
  return "Sends to";
}

function destinationHelp(row: AppIntegrationRow): string {
  if (row.kind === "email") {
    return "Mail goes out as “{app name} (via Toft)” from this install’s address.";
  }
  if (row.leaves_residue) {
    return "Anyone with access to that folder can read these files. Files are added, never overwritten.";
  }
  // Deliberately only about what *this app* sends. Whether Toft's own
  // notices also go here is the connection's business and is said on its page;
  // claiming it on a kind that carries no platform audience would be wrong, and
  // this row does not carry that fact.
  return "Everyone who can see that destination can read what this app sends.";
}

function IntegrationCard({
  appId,
  row,
  data,
  isAdmin,
}: {
  appId: string;
  row: AppIntegrationRow;
  data: AppIntegrations;
  isAdmin: boolean;
}) {
  const queryClient = useQueryClient();
  const [confirmDetach, setConfirmDetach] = useState(false);
  const [replyTo, setReplyTo] = useState(row.destination.id);
  // Which connection this attach goes through, where there is a choice.
  const [chosenConnection, setChosenConnection] = useState("");

  // Only asked for once the app is attached: the attachment is what makes the
  // organization's list visible to an app's people at all.
  const {
    data: dests, isLoading: destsLoading, error: destsError,
  } = useQuery({
    queryKey: ["app-integration-destinations", appId, row.kind],
    queryFn: () => api.listAppIntegrationDestinations(appId, row.kind),
    enabled: row.attached && row.enabled && row.enumerates,
    retry: false,
  });
  // An empty menu with no explanation reads as "this app cannot have one". It
  // means one of two fixable things, so say which.
  //
  // **And a read that has not answered is neither.** `dests?.destinations
  // .length ?? 0` is 0 for the whole round trip, so every arrival on this tab
  // asserted that the platform could reach no channel — then replaced it with
  // a working dropdown a moment later. Three states: not answered, failed,
  // answered with nothing.
  const menuUnavailable =
    !destsLoading && (!!destsError || (dests?.destinations.length ?? 0) === 0);

  // The organization's connections, read only to answer "which one". An attach
  // that names none is refused by the server the moment this install has two
  // of a kind — "say which: ops-relay, alerts" — and the answer has to be
  // askable *before* the click, not offered after a red toast.
  const { data: connectionData } = useQuery({
    queryKey: ["integration-connections"],
    queryFn: api.listIntegrationConnections,
    enabled: isAdmin && !row.attached,
    retry: false,
  });
  // Only the not-yet-attached state offers this. One attachment per (app,
  // kind), and a second is refused rather than swapped — so a picker on an
  // attached row would be a re-target the server is right to refuse.
  const ofKind = (connectionData?.connections ?? []).filter(
    (c) => c.kind === row.kind && c.for_apps,
  );

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ["app-integrations", appId] });
    queryClient.invalidateQueries({ queryKey: ["app", appId] });
  };

  const attach = useMutation({
    mutationFn: (enabled: boolean) =>
      api.attachAppIntegration(appId, row.kind, {
        enabled,
        // Omitted where there is nothing to choose, which is the shape every
        // single-connection install sends and the server already resolves.
        ...(enabled && chosenConnection ? { connection_id: chosenConnection } : {}),
      }),
    onSuccess: (_r, enabled) => {
      invalidate();
      toast.success(
        enabled
          ? `This app can send through ${row.title}`
          : `${row.title} is switched off for this app`,
      );
    },
    onError: (e: Error) => toast.error(e.message, { duration: 8000 }),
  });

  const detach = useMutation({
    mutationFn: () => api.detachAppIntegration(appId, row.kind),
    onSuccess: () => {
      invalidate();
      setConfirmDetach(false);
      toast.success(`${row.title} removed from this app`);
    },
    onError: (e: Error) => toast.error(e.message, { duration: 8000 }),
  });

  const setDestination = useMutation({
    mutationFn: (id: string) => api.setAppIntegrationDestination(appId, row.kind, id),
    onSuccess: (_r, id) => {
      invalidate();
      toast.success(
        id
          ? `This app sends to ${id}`
          : `This app sends to the default ${row.destination_label ?? "destination"}`,
      );
    },
    onError: (e: Error) => toast.error(e.message, { duration: 8000 }),
  });

  const defaultLabel = row.default_destination.label || row.default_destination.id;

  // Which connection an unattached row would attach through, named.
  const throughName = row.connection_name
    ? `${row.connection_name}${row.scope_label ? ` (${row.scope_label})` : ""}`
    : ofKind.length === 1
      ? `${ofKind[0].name}${ofKind[0].scope_label ? ` (${ofKind[0].scope_label})` : ""}`
      : "";

  // **One row per integration, and its state is the loudest thing on it.**
  // Each kind was a heading over a card whose first line was a muted state
  // sentence; five kinds ran to 1,900px and which were on was the quietest
  // text on the page. Name, one line of what it sends, a badge and the action
  // at the right — Admin → Integrations' *What you can connect* row — and an
  // attached integration opens in place to its settings.
  return (
    <div className="space-y-4 px-4 py-3">
      <div className="flex items-center gap-3">
        <div className="min-w-0 flex-1">
          <span className="text-sm font-medium">{row.title}</span>
          <p className="text-xs text-muted-foreground">
            {sendSummaries(row)}
            {!row.attached && isAdmin && throughName && <> Through “{throughName}”.</>}
          </p>
        </div>
        <StatusBadge
          status=""
          tone={row.attached && row.enabled ? "ok" : "neutral"}
          label={row.attached && row.enabled ? "On" : "Off"}
        />
        {isAdmin && !row.attached && (
          // Outlined: a page of unattached kinds was a column of filled
          // buttons, one primary per row, which is none.
          <Button
            size="sm"
            variant="outline"
            className="shrink-0"
            onClick={() => attach.mutate(true)}
            disabled={attach.isPending || (ofKind.length > 1 && chosenConnection === "")}
          >
            {attach.isPending ? "Enabling…" : "Enable"}
          </Button>
        )}
        {isAdmin && row.attached && (
          <Switch
            checked={row.enabled}
            onCheckedChange={(v) => attach.mutate(v)}
            disabled={attach.isPending}
            aria-label={`${row.title} enabled for this app`}
          />
        )}
      </div>

      {!row.attached && isAdmin && (
        <>
        {ofKind.length === 1 && ofKind[0].description && (
          <p className="text-xs text-muted-foreground">{ofKind[0].description}</p>
        )}
        {(ofKind.length === 1
          ? ofKind[0]
          : ofKind.find((c) => c.id === chosenConnection)
        )?.issue_tokens && (
          <Notice tone="attention" size="sm" title="This connection hands out access tokens">
            The app will be able to get the connection’s own token, which reaches its
            whole grant, not only the folder chosen here.
          </Notice>
        )}
        {row.leaves_residue && (
          <Notice tone="attention" size="sm" title="This one leaves files behind">
            Files the app writes stay, even after the integration or the app is removed.
          </Notice>
        )}
        {ofKind.length > 1 && (
          <div className="space-y-1.5">
            <Label htmlFor={`conn-${row.kind}`}>Send through</Label>
            <Select
              value={chosenConnection}
              onValueChange={(v) => setChosenConnection(v ?? "")}
            >
              <SelectTrigger id={`conn-${row.kind}`} className="w-full sm:w-80">
                <SelectValue>
                  {(v) =>
                    ofKind.find((c) => c.id === v)?.name ?? "Choose a connection…"
                  }
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                {ofKind.map((c) => (
                  <SelectItem key={c.id} value={c.id}>
                    <span className="block">{c.name}</span>
                    {c.scope_label && (
                      <span className="block text-xs text-muted-foreground">
                        {c.scope_label}
                      </span>
                    )}
                    {c.description && (
                      <span className="block max-w-72 truncate text-xs text-muted-foreground">
                        {c.description}
                      </span>
                    )}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        )}
        </>
      )}

      {row.attached && !row.connected && (
        <Notice size="sm">This app’s connection is switched off, so nothing is sent.</Notice>
      )}

      {row.attached && !row.enabled && !isAdmin && (
        <p className="text-xs text-muted-foreground">
          An administrator has switched {row.title} off for this app. Nothing is sent.
        </p>
      )}

      {row.attached && row.enabled && (
        <>
          {row.destination_label && (
            <>
              <div className="space-y-1">
                <p className="text-sm font-medium">{destinationHeading(row)}</p>
                <p className="text-xs text-muted-foreground">
                  {destinationHelp(row)}
                </p>
              </div>

              {row.enumerates ? (
                destsLoading ? (
                  <Skeleton className="h-9 w-full" />
                ) : menuUnavailable ? (
                  <Notice
                    title={
                      destsError
                        ? (destsError as Error).message
                        : `Toft can’t reach any ${row.destination_label} yet`
                    }
                  >
                    Until then this app sends to the default
                    {defaultLabel ? ` (${defaultLabel})` : ""}.
                    {row.kind === "slack" && (
                      <>
                        {" "}Run <Code>/invite @Toft</Code> in the channel you want,
                        then reload this page.
                      </>
                    )}
                  </Notice>
                ) : (
                  <>
                    <DestinationSelect
                      value={row.destination.id}
                      onChange={(v) => setDestination.mutate(v)}
                      destinations={dests?.destinations ?? []}
                      disabled={setDestination.isPending || !row.connected}
                      placeholder={`Choose a ${row.destination_label}…`}
                      hash={row.kind === "slack"}
                      defaultLabel={
                        defaultLabel
                          ? `Use the default (${defaultLabel})`
                          : "Use the default"
                      }
                    />
                  </>
                )
              ) : (
                <div className="flex gap-2">
                  <Input
                    id={`dest-${row.kind}`}
                    value={replyTo}
                    onChange={(e) => setReplyTo(e.target.value)}
                    placeholder={data.reply_to_fallback || (data.reply_to_fallback_kind === "install" ? "this install’s reply-to" : "this app’s owner")}
                    aria-label={`Default ${row.destination_label}`}
                  />
                  {/* Only once edited — the self-saving-field rule. */}
                  {replyTo !== row.destination.id && (
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => setDestination.mutate(replyTo)}
                      disabled={setDestination.isPending}
                    >
                      {setDestination.isPending ? "Saving…" : "Save"}
                    </Button>
                  )}
                </div>
              )}
              {!row.enumerates && row.kind === "email" && (
                <p className="text-xs text-muted-foreground">
                  {data.reply_to_fallback_kind === "install"
                    ? `Replies go to this install’s reply-to address${
                        data.reply_to_fallback ? ` (${data.reply_to_fallback})` : ""
                      } because a group owns this app.`
                    : `Leave blank to send replies to this app’s owner${
                        data.reply_to_fallback ? ` (${data.reply_to_fallback})` : ""
                      }.`}
                </p>
              )}
            </>
          )}

          {row.issues_tokens && (
            <Notice tone="info" size="sm" title="This app can get access tokens">
              Its connection, “{row.connection_name}”, hands apps its own token. The token
              reaches everything the connection’s grant does, not only this folder.
            </Notice>
          )}

          <TestButton
            appId={appId}
            kind={row.kind}
            title={row.title}
            carriesFile={row.actions[0]?.carries === "file"}
          />
        </>
      )}

      {row.attached && isAdmin && (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-xs text-muted-foreground">
            When off, every send stops, including queued ones. Settings are kept.
          </p>
          {/* A plain confirm, which is the tier this act belongs to: a
              capability an administrator can re-enable in three clicks. */}
          <Button variant="destructive" size="sm" onClick={() => setConfirmDetach(true)}>
            Remove from this app…
          </Button>
          <Dialog open={confirmDetach} onOpenChange={setConfirmDetach}>
            <DialogContent>
              <DialogHeader>
                <DialogTitle>Remove {row.title} from this app?</DialogTitle>
                <DialogDescription>
                  The app loses {row.title} and its settings. Past sends stay in the log
                  {row.leaves_residue ? ", and files it wrote stay where they are" : ""}.
                </DialogDescription>
              </DialogHeader>
              <DialogFooter>
                <Button variant="outline" onClick={() => setConfirmDetach(false)}>
                  Cancel
                </Button>
                <Button
                  variant="destructive"
                  onClick={() => detach.mutate()}
                  disabled={detach.isPending}
                >
                  {detach.isPending ? "Removing…" : "Remove"}
                </Button>
              </DialogFooter>
            </DialogContent>
          </Dialog>
        </div>
      )}
    </div>
  );
}

/**
 * A real send through this app's own identity — the same gates, the same
 * record. A test that bypassed either would prove the wrong thing.
 */
function TestButton({
  appId,
  kind,
  title,
  carriesFile,
}: {
  appId: string;
  kind: string;
  title: string;
  /** A file-only kind is tested by the one thing it can do: a one-line
   *  text file, toft-test.txt, lands in the default destination. */
  carriesFile: boolean;
}) {
  const queryClient = useQueryClient();
  const test = useMutation({
    mutationFn: () => api.testAppIntegration(appId, kind),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["app-sends", appId] });
      toast.success(
        kind === "email"
          ? "Test sent to your address"
          : carriesFile
            ? `Uploaded toft-test.txt through ${title}`
            : `Test sent through ${title}`,
      );
    },
    onError: (e: Error) => toast.error(e.message, { duration: 8000 }),
  });

  return (
    <Button
      variant="outline"
      size="sm"
      onClick={() => test.mutate()}
      disabled={test.isPending}
    >
      <Send className="mr-1.5 h-3.5 w-3.5" />
      {test.isPending ? "Sending…" : carriesFile ? "Upload a test file" : "Send a test"}
    </Button>
  );
}

/** Where an owner answers "did it go out". */
/** How many sends this section asks for, and the ceiling the server serves. */
const SENDS_PAGE = 20;
const SENDS_MAX = 200;

function RecentSends({
  appId,
  titles,
}: {
  appId: string;
  /** Each kind's catalog title, so a row never prints the raw kind. */
  titles: Record<string, string>;
}) {
  const [limit, setLimit] = useState(SENDS_PAGE);
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["app-sends", appId, limit],
    queryFn: () => api.listAppSends(appId, limit),
    refetchInterval: 15_000,
  });
  const sends = data?.sends ?? [];
  // A full page is the signal there is more: the server answers a `LIMIT` and
  // says nothing about what it left. Not a claim that more exists — the words
  // below say "the most recent N", which is true either way.
  const maybeMore = sends.length === limit && limit < SENDS_MAX;

  return (
    <section className="space-y-4">
      {/* What it holds, not what the table holds. "Every message this app
          asked Toft to send" over the last twenty is a claim the section
          cannot keep, and the twenty is a `LIMIT`, not a retention. */}
      <SectionHeading
        title="Recent sends"
        description={(() => {
          const n = sends.length || SENDS_PAGE;
          return n === 1
            ? "The last message this app asked to send, delivered or not."
            : `The last ${n} messages this app asked to send, delivered or not.`;
        })()}
      />

      {isLoading ? (
        <Skeleton className="h-32 w-full" />
      ) : error ? (
        // This record is the thing an owner comes here to trust. Reporting a
        // failed read as "nothing sent yet" is the one answer it must never
        // give.
        <QueryError
          title="The send record could not be read"
          error={error}
          fallback="Only the app’s owner and editors can see what it sent."
          onRetry={() => void refetch()}
        />
      ) : sends.length === 0 ? (
        <EmptyState
          icon={<Send className="h-6 w-6" />}
          title="Nothing sent yet"
          description="Messages this app sends through the Toft SDK appear here."
        />
      ) : (
        <div className="space-y-2">
        <TableCard>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>When</TableHead>
                <TableHead>Through</TableHead>
                <TableHead className="hidden @xl:table-cell">Went to</TableHead>
                <TableHead className="hidden @3xl:table-cell">Subject</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {sends.map((s) => (
                <SendRow key={s.id} send={s} title={titles[s.kind] ?? s.kind} />
              ))}
            </TableBody>
          </Table>
        </TableCard>
        {/* The way to the rest, where there is one. The section is capped by a
            `LIMIT` rather than by retention, so "no more to show" is something
            only the server can say and this does not claim it. */}
        {maybeMore && (
          <Button
            variant="outline"
            size="sm"
            onClick={() => setLimit((n) => Math.min(n * 5, SENDS_MAX))}
          >
            Show more
          </Button>
        )}
        </div>
      )}
    </section>
  );
}

function SendRow({ send, title }: { send: IntegrationSend; title: string }) {
  return (
    <TableRow>
      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
        <RelativeTime iso={send.created_at} />
      </TableCell>
      <TableCell className="text-sm">
        {title}
        {send.action === "file" ? " file" : ""}
      </TableCell>
      <TableCell className="hidden max-w-[18rem] truncate text-sm @xl:table-cell">
        {send.recipients.length > 0
          ? send.recipients.join(", ")
          : send.destination || "Default destination"}
      </TableCell>
      <TableCell className="hidden max-w-[16rem] truncate text-sm @3xl:table-cell">
        {send.subject || <span className="text-muted-foreground">—</span>}
      </TableCell>
      <TableCell>
        <div className="space-y-1">
          <SendStatusBadge status={send.status} />
          {send.error && (
            <p className="max-w-[20rem] text-xs text-muted-foreground">{send.error}</p>
          )}
        </div>
      </TableCell>
    </TableRow>
  );
}

/** What attaching lets an app send. The token action is said separately, and
 * only where the connection hands tokens out. */
function sendSummaries(row: AppIntegrationRow): string {
  return row.actions
    .filter((a) => a.carries !== "nothing")
    .map((a) => a.summary)
    .join(" ");
}
