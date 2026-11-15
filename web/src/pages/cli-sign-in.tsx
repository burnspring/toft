import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";
import { Check, Loader2, X } from "lucide-react";
import { toast } from "sonner";

import { api } from "@/lib/api";
import { useAuth } from "@/hooks/use-auth";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Notice } from "@/components/notice";
import { PublicFrame } from "@/components/layout/public-frame";
import { TableCard } from "@/components/table-card";
import { Skeleton } from "@/components/ui/skeleton";
import { signInCopy, type SignInCopy } from "@/lib/sign-in-client";
import { useDocumentTitle } from "@/hooks/use-document-title";

/**
 * The browser half of `toft login`.
 *
 * A terminal asked to sign in and printed eight characters. This page is where
 * a person confirms that the terminal in question is theirs — and it is the
 * entire authorization for minting a credential, which is why the page does one
 * thing and says exactly what it is doing.
 *
 * **The machine that asked is the point.** The code alone proves nothing: the
 * risk this flow carries, and accepts, is that somebody can be talked into
 * approving a code they did not generate. So the screen leads with the hostname
 * the client reported and says in one line to deny it if they did not just run
 * `toft login` there. That sentence is the mitigation; it is not filler.
 *
 * Not under the Layout, and deliberately so. This is a decision about a
 * credential, taken in one moment, and a page with a nav bar invites the reader
 * to be somewhere else. It wears the login page's frame for the same reason,
 * with the wordmark left unlinked: nothing on the screen leads away from the
 * answer.
 */
export function CliSignInPage() {
  const [params, setParams] = useSearchParams();
  const { user } = useAuth();

  // The code arrives in the URL from `verification_uri_complete`, or is typed
  // by somebody who opened /cli themselves. Both are ordinary.
  const urlCode = params.get("code") ?? "";
  const [typed, setTyped] = useState(urlCode);
  const [answered, setAnswered] = useState<"approved" | "denied" | null>(null);
  /** Which button is in flight, not merely that one is — a single boolean put
   * "Approving…" on the Approve button when the person had pressed Deny. */
  const [busy, setBusy] = useState<"approve" | "deny" | null>(null);

  // A new code in the URL replaces whatever was typed, adjusted during render
  // rather than in an effect so the old text is never drawn beside the new code.
  const [typedFor, setTypedFor] = useState(urlCode);
  if (typedFor !== urlCode) {
    setTypedFor(urlCode);
    setTyped(urlCode);
  }

  const { data, isLoading, error } = useQuery({
    queryKey: ["cli-sign-in", urlCode],
    queryFn: () => api.getCliSignIn(urlCode),
    enabled: urlCode !== "",
    // A pending sign-in dies in ten minutes and nothing here should be served
    // from a cache that outlives it.
    staleTime: 0,
    retry: false,
  });
  // `toft login` or RStudio — the same decision, worded for the act the reader
  // should have just performed (ADR §102).
  const copy = signInCopy(data?.client);
  useDocumentTitle(copy.title);

  async function answer(approve: boolean) {
    setBusy(approve ? "approve" : "deny");
    try {
      await api.answerCliSignIn(urlCode, approve);
      setAnswered(approve ? "approved" : "denied");
      toast.success(approve ? copy.approvedToast : copy.deniedToast);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t answer this sign-in");
    } finally {
      setBusy(null);
    }
  }

  return (
    <PublicFrame footer={false} homeLink={false}>
      <div className="mx-auto grid w-full max-w-5xl flex-1 content-center gap-10 px-4 py-14 sm:px-6 sm:py-20 lg:grid-cols-2 lg:items-center lg:gap-16">
        <div>
          <h1 className="text-4xl leading-[1.05] font-semibold tracking-tighter text-balance sm:text-5xl">
            {copy.title}
          </h1>
          <p className="mt-4 text-lg leading-relaxed text-muted-foreground">
            Signed in as <span className="font-medium text-foreground">{user?.login}</span>
          </p>
        </div>

        <TableCard className="w-full p-6 sm:p-8">
          {answered ? (
            <Answered outcome={answered} copy={copy} />
          ) : urlCode === "" ? (
            <CodeEntry
              value={typed}
              onChange={setTyped}
              onSubmit={() => setParams({ code: typed })}
            />
          ) : isLoading ? (
            <div className="space-y-3">
              <Skeleton className="h-4 w-32" />
              <Skeleton className="h-16 w-full" />
            </div>
          ) : error ? (
            // The server's own words: it says whether the code is unknown,
            // expired or mistyped, and it phrases that better than a generic
            // fallback could.
            <div className="space-y-4">
              <Notice tone="danger" title="Couldn’t find that code">
                {error instanceof Error ? error.message : "The sign-in could not be found."}
              </Notice>
              <Button variant="outline" className="w-full" onClick={() => setParams({})}>
                Enter a different code
              </Button>
            </div>
          ) : data && data.status !== "pending" ? (
            <Notice tone="neutral" title="Nothing to approve">
              {data.status === "expired" ? copy.expired : "This sign-in has already been answered."}
            </Notice>
          ) : data ? (
            <Approval
              hostname={data.hostname}
              clientVersion={data.client_version}
              requestedFrom={data.requested_from}
              copy={copy}
              userCode={data.user_code}
              busy={busy}
              onAnswer={answer}
            />
          ) : null}
        </TableCard>
      </div>
    </PublicFrame>
  );
}

/** Somebody opened /cli without a code. Ask for it rather than showing them a
 * dead end — the terminal printed it two lines above the URL. */
function CodeEntry({
  value,
  onChange,
  onSubmit,
}: {
  value: string;
  onChange: (v: string) => void;
  onSubmit: () => void;
}) {
  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault();
        if (value.trim()) onSubmit();
      }}
    >
      <div className="space-y-2">
        <Label htmlFor="user-code">Enter the code from your terminal</Label>
        <Input
          id="user-code"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder="TFXK-4T2Q"
          autoFocus
          autoComplete="off"
          spellCheck={false}
          className="text-center font-mono text-lg tracking-[0.3em] uppercase"
        />
      </div>
      <Button type="submit" className="w-full" disabled={!value.trim()}>
        Continue
      </Button>
    </form>
  );
}

/**
 * The decision itself.
 *
 * The hostname is a string the client chose, rendered as text like every other
 * piece of untrusted display copy in this product, and bounded by the server so
 * it cannot push the sentence below it off the card.
 */
function Approval({
  hostname,
  clientVersion,
  requestedFrom,
  copy,
  userCode,
  busy,
  onAnswer,
}: {
  hostname: string;
  clientVersion: string;
  requestedFrom: string;
  copy: SignInCopy;
  userCode: string;
  busy: "approve" | "deny" | null;
  onAnswer: (approve: boolean) => void;
}) {
  return (
    <div className="space-y-5">
      <dl className="space-y-3 rounded-md border border-border/60 bg-muted/40 p-4 text-sm">
        <div className="flex items-baseline justify-between gap-4">
          <dt className="text-muted-foreground">Code</dt>
          <dd className="font-mono tracking-wider">{userCode}</dd>
        </div>
        {(copy.reportsHostname || hostname) && (
          <div className="flex items-baseline justify-between gap-4">
            <dt className="shrink-0 text-muted-foreground">Machine</dt>
            {/* Wraps rather than truncates. This is the one line the reader is
                being asked to check, and an ellipsis hides exactly the end of a
                hostname that a lookalike would differ in. The server bounds the
                string, so wrapping costs at most a second line. */}
            <dd className="font-mono break-all text-right">{hostname || "not reported"}</dd>
          </div>
        )}
        {/* What the server saw rather than what the client said: the one row
            here a lookalike cannot choose. Absent on a request recorded before
            the server kept it. */}
        {requestedFrom && (
          <div className="flex items-baseline justify-between gap-4">
            <dt className="shrink-0 text-muted-foreground">Requested from</dt>
            <dd className="font-mono break-all text-right">{requestedFrom}</dd>
          </div>
        )}
        {clientVersion && (
          <div className="flex items-baseline justify-between gap-4">
            <dt className="text-muted-foreground">Client</dt>
            <dd className="font-mono break-all text-right">
              {copy.clientLabel ? `${copy.clientLabel} ${clientVersion}` : clientVersion}
            </dd>
          </div>
        )}
      </dl>

      <Notice
        tone="attention"
        size="sm"
        title={copy.warningTitle}
        live
      >
        Approving gives that machine an API key that acts as you — it can deploy, roll back and
        read anything you can. If you did not just {copy.act}, deny it.
      </Notice>

      <div className="flex gap-2">
        <Button className="flex-1" onClick={() => onAnswer(true)} disabled={busy !== null}>
          {busy === "approve" ? (
            <Loader2 className="h-4 w-4 animate-spin" />
          ) : (
            <Check className="h-4 w-4" />
          )}
          {busy === "approve" ? "Approving…" : "Approve"}
        </Button>
        <Button
          variant="outline"
          className="flex-1"
          onClick={() => onAnswer(false)}
          disabled={busy !== null}
        >
          {busy === "deny" ? (
            <Loader2 className="h-4 w-4 animate-spin" />
          ) : (
            <X className="h-4 w-4" />
          )}
          {busy === "deny" ? "Denying…" : "Deny"}
        </Button>
      </div>
    </div>
  );
}

/** After the decision. It says what to do next in each case, because the person
 * is about to look at a terminal that has either moved on or has not. */
function Answered({ outcome, copy }: { outcome: "approved" | "denied"; copy: SignInCopy }) {
  if (outcome === "approved") {
    return (
      <div className="space-y-4">
        <Notice tone="ok" title="Signed in" live>
          {copy.approved} You can revoke it at any time under Your account → Credentials.
        </Notice>
        <a
          href="/me/credentials"
          className="block text-center text-sm text-muted-foreground underline"
        >
          See your API keys
        </a>
      </div>
    );
  }
  return (
    <Notice tone="neutral" title="Denied" live>
      {copy.denied} If it wasn’t yours, there is nothing more to do — the code is dead and cannot
      be reused.
    </Notice>
  );
}
