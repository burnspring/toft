import { useQuery } from "@tanstack/react-query";
import {
  Ban,
  Clock,
  KeyRound,
  Loader2,
  LogOut,
  ShieldAlert,
  UserX,
  Users,
} from "lucide-react";
import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { toast } from "sonner";

import { api, type LoginOptions } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { buttonVariants } from "@/components/ui/button-variants";
import { PublicFrame } from "@/components/layout/public-frame";
import { TableCard } from "@/components/table-card";
import { Notice } from "@/components/notice";
import { Disclosure } from "@/components/disclosure";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { FieldHelp } from "@/components/field-help";
import { useDocumentTitle } from "@/hooks/use-document-title";

/**
 * Sign-in.
 *
 * The page renders whatever the install offers rather than a hardcoded
 * provider, and when there is exactly one way in it does not render at all — it
 * forwards. A typical enterprise install has a single corporate IdP, and a page
 * whose only content is one button is a click that exists for no reason.
 *
 * The *server* decides whether to forward (`auto_select`), not this component:
 * that keeps the rule in one place, and a second provider appearing stops the
 * forwarding with no change here.
 */
export function LoginPage() {
  // A protected app's gateway sends visitors here with ?redirect=<app URL>;
  // pass it through — the server validates it against its origin allowlist.
  const [params] = useSearchParams();
  const redirect = params.get("redirect") || window.location.origin + "/";
  const providerError = params.get("error");
  // A refusal by *this* platform rather than by the provider: the account is
  // locked, or it does not exist and this install does not create accounts.
  // Either way the person authenticated fine, so the page leads with the
  // explanation instead of another sign-in button.
  const denied = params.get("denied");
  // The session ran out rather than never existing. `lib/api.ts` sets this on
  // the 401 that discovered it, and only when this tab had been signed in —
  // so it is never shown to somebody arriving at the login page cold.
  const expired = params.get("expired") === "1";
  // The person just pressed Sign out. Not a refusal, but like one it stops the
  // forward: the provider's own session outlives ours, so forwarding would
  // sign them back in without a click (App.tsx sets this).
  const signedOut = params.get("signed_out") === "1";

  useDocumentTitle("Sign in");

  const { data, isLoading, error } = useQuery({
    queryKey: ["login-options"],
    queryFn: api.getLoginOptions,
    // The way in must not be cached across a provider change an admin just made.
    staleTime: 0,
  });

  const loginURL = (id: string) =>
    `/api/v1/auth/${encodeURIComponent(id)}/login?redirect=${encodeURIComponent(redirect)}`;

  // Forward on the single-provider install. Suppressed when the provider
  // returned an error, and when this platform refused the sign-in: bouncing
  // someone straight back to the IdP that just authenticated them would loop
  // them through a refusal they never get to read. And after a sign-out, when
  // the provider's still-live session would answer without asking.
  const autoSelect =
    providerError || denied || signedOut ? undefined : data?.auto_select;
  useEffect(() => {
    if (autoSelect) window.location.href = loginURL(autoSelect);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [autoSelect]);

  return (
    <PublicFrame footer={false}>
      <div className="mx-auto grid w-full max-w-5xl flex-1 content-center gap-10 px-4 py-14 sm:px-6 sm:py-20 lg:grid-cols-2 lg:items-center lg:gap-16">
        <div>
          <h1 className="text-4xl leading-[1.05] font-semibold tracking-tighter text-balance sm:text-5xl">
            Sign in to Toft
          </h1>
          {/* The landing page's line, not a second one. A visitor met
              "where your organization’s own software lives" and then, one
              click later, "where your internal tools go to run" — two
              descriptions of the same product, seconds apart, in the first
              two screens anybody sees. It stands here rather than in the
              frame's footer, which this page leaves off so it is said once. */}
          <p className="mt-4 max-w-md text-lg leading-relaxed text-pretty text-muted-foreground">
            Where your organization’s own software lives
          </p>
        </div>

        <TableCard className="w-full p-6 sm:p-8">
          {denied && (
            <DeniedNotice
              reason={denied}
              message={
                denied === "locked"
                  ? data?.locked_message
                  : denied === "pending_activation"
                    ? data?.pending_activation_message
                    : data?.no_account_message
              }
            />
          )}

          {/* Not shown beside a refusal: a locked account and an expired
              session are two different answers to "why am I here", and the
              refusal is the one that needs reading. */}
          {expired && !denied && (
            <Notice
              className="mb-6"
              tone="neutral"
              icon={<Clock className="h-4 w-4 shrink-0" />}
            >
              Your session ended. Sign in again.
            </Notice>
          )}

          {signedOut && !denied && !providerError && (
            <Notice
              className="mb-6"
              tone="neutral"
              icon={<LogOut className="h-4 w-4 shrink-0" />}
            >
              You signed out.
            </Notice>
          )}

          {providerError && (
            <Notice
              className="mb-6"
              icon={<ShieldAlert className="h-4 w-4 shrink-0" />}
            >
              {providerErrorMessage(providerError)}
            </Notice>
          )}

          {isLoading || autoSelect ? (
            <div className="flex items-center gap-2 py-6 text-sm text-muted-foreground">
              <Loader2 className="h-4 w-4 animate-spin" />
              {autoSelect
                ? "Redirecting to your sign-in provider…"
                : "Loading sign-in options…"}
            </div>
          ) : error ? (
            <p className="py-4 text-sm text-muted-foreground">
              Couldn’t load sign-in options. Reload the page, or contact an
              administrator.
            </p>
          ) : (
            <SignInOptions
              options={data}
              loginURL={loginURL}
              redirect={redirect}
            />
          )}

          {/* The forwarded case still gets a link: a browser that blocks the
              redirect, or a slow IdP, otherwise leaves a spinner and no way
              forward. */}
          {autoSelect && (
            <p className="mt-4 text-xs text-muted-foreground">
              Not redirected?{" "}
              <a href={loginURL(autoSelect)} className="underline underline-offset-2 hover:text-foreground">
                Continue manually
              </a>
            </p>
          )}
        </TableCard>
      </div>
    </PublicFrame>
  );
}

/**
 * OAuth error codes are system vocabulary; say what happened in plain words.
 * Unknown codes get the generic line rather than leaking the code as the
 * message — it still appears in the URL for whoever debugs it.
 */
function providerErrorMessage(code: string): string {
  switch (code) {
    case "access_denied":
      return "Sign-in was cancelled or refused by the provider. Try again if that wasn’t you.";
    case "temporarily_unavailable":
      return "The sign-in provider is temporarily unavailable. Try again in a moment.";
    default:
      return "Couldn’t sign you in with that provider. Try again, or contact an administrator if it continues.";
  }
}

/**
 * Why this person is not getting in, in the administrator's words.
 *
 * The wording is theirs and not this component's: the platform knows the
 * account is locked, or absent, and only the organization can say what to do
 * about it. The heading is ours because it has to be true on every install, and
 * because the message is still loading for the first moment the page is up.
 *
 * Rendered as text. It is admin-authored copy, not markup — an escape hatch
 * into HTML here would be a stored-XSS surface on the one page every
 * unauthenticated visitor sees.
 */
/**
 * The three answers somebody can be given after authenticating perfectly well.
 *
 * They are kept apart rather than folded into one "you cannot sign in", because
 * what the person should do next differs in each: a locked account is over
 * until somebody reverses it, no account is a request to make, and a pending
 * one is already made and only has to be waited on. The tones follow the same
 * reading — a queue is not a failure, so it is `info` rather than the amber
 * that would send somebody looking for a fault.
 */
function DeniedNotice({
  reason,
  message,
}: {
  reason: string;
  message?: string;
}) {
  const locked = reason === "locked";
  const pending = reason === "pending_activation";
  // The group gate's copy is the platform's own rather than an administrator's:
  // it is the one refusal here that is usually about configuration and not
  // about the person, and there is no settings field behind it.
  //
  // One sentence for both of the gate's refusals. Whether the claim arrived and
  // matched nothing or never arrived at all is an administrator's problem, and
  // saying which would tell somebody probing the login page which half of the
  // policy they tripped.
  const groupRefused = reason === "group_not_permitted";
  return (
    <Notice
      className="mb-6"
      tone={locked ? "danger" : pending ? "info" : "attention"}
      icon={
        locked ? (
          <Ban className="h-4 w-4 shrink-0" />
        ) : pending ? (
          <Clock className="h-4 w-4 shrink-0" />
        ) : groupRefused ? (
          <Users className="h-4 w-4 shrink-0" />
        ) : (
          <UserX className="h-4 w-4 shrink-0" />
        )
      }
      title={
        locked
          ? "Your account is locked"
          : pending
            ? "Your account is waiting to be activated"
            : groupRefused
              ? "Your directory groups do not permit sign-in here"
              : "You do not have an account here"
      }
    >
      {groupRefused ? (
        <p className="leading-relaxed">
          Sign-in through this provider is limited to members of certain groups
          in your organization’s directory. Your credentials were fine — ask an
          administrator to check whether you should be in one of those groups.
        </p>
      ) : (
        message && (
          <p className="leading-relaxed whitespace-pre-line">{message}</p>
        )
      )}
    </Notice>
  );
}

function SignInOptions({
  options,
  loginURL,
  redirect,
}: {
  options?: LoginOptions;
  loginURL: (id: string) => string;
  redirect: string;
}) {
  const providers = options?.providers ?? [];
  // Identity providers lead; the local password form goes last. An install that
  // has SSO working wants the corporate door first, and the built-in
  // administrator is the thing you use before that is true — or when it breaks.
  const external = providers.filter((p) => p.kind !== "password");
  const local = providers.find((p) => p.kind === "password");

  // Nothing at all means nobody can sign in. Say so plainly, with the fix — the
  // alternative is a login page with nothing on it, which reads as a broken
  // deployment rather than an unfinished one.
  if (providers.length === 0) {
    return (
      <div className="rounded-md border border-border/60 bg-muted/40 p-4 text-xs text-muted-foreground">
        <p className="mb-2 font-medium text-foreground">
          No sign-in provider is enabled.
        </p>
        <p>
          An operator sets one with{" "}
          <code className="font-mono">OIDC_ISSUER</code>,{" "}
          <code className="font-mono">OIDC_CLIENT_ID</code> and{" "}
          <code className="font-mono">OIDC_CLIENT_SECRET</code> (any OpenID
          Connect provider), or{" "}
          <code className="font-mono">GITHUB_CLIENT_ID</code> and{" "}
          <code className="font-mono">GITHUB_CLIENT_SECRET</code>. To sign in as
          the built-in administrator instead, run{" "}
          <code className="font-mono">toft local-admin enable</code> on the
          server.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-2.5">
      {external.map((p, i) => (
        <a
          key={p.id}
          href={loginURL(p.id)}
          className={buttonVariants({
            size: "lg",
            variant: i === 0 ? "default" : "outline",
            className: "w-full gap-2",
          })}
        >
          {p.kind === "github" ? (
            <GitHubMark />
          ) : (
            <KeyRound className="h-4 w-4" />
          )}
          Continue with {p.display_name}
        </a>
      ))}

      {/* **Enterprise-first, and collapsed.** The rule the rest of the product
          follows — Admin → Authentication is the reference — is that the
          corporate door leads and the small-install convenience goes below it,
          behind a disclosure. This page had the provider button and the local
          form's Sign in as two solid primary buttons of equal weight, one above
          the other, competing on the first screen anybody sees.

          The built-in administrator is what you use *before* SSO works, or when
          it breaks; it is not one of two equal ways in. So where there is a
          provider it folds away, and where there is not it is the whole page and
          stays open — an install with no SSO would otherwise open on a
          disclosure and nothing else. */}
      {local &&
        (external.length > 0 ? (
          <Disclosure
            label={`Sign in as the ${local.display_name.toLowerCase()}`}
            className="pt-2"
          >
            <PasswordForm
              displayName={local.display_name}
              redirect={redirect}
            />
          </Disclosure>
        ) : (
          <PasswordForm displayName={local.display_name} redirect={redirect} />
        ))}
    </div>
  );
}

/**
 * Local sign-in for the built-in administrator.
 *
 * Two steps, because a fresh install ships with a published default password and
 * verifying it mints no session — only the right to replace it. So the form
 * either signs you in or turns into "choose a new password", and the second state
 * is not a separate route: the current password is the credential authenticating
 * the change, and it is in this component's state.
 */
function PasswordForm({
  displayName,
  redirect,
}: {
  displayName: string;
  redirect: string;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [mustChange, setMustChange] = useState<string | undefined>();
  const [pending, setPending] = useState(false);
  const [failure, setFailure] = useState<string | undefined>();

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFailure(undefined);

    if (mustChange && newPassword !== confirmPassword) {
      setFailure("The two new passwords do not match.");
      return;
    }

    setPending(true);
    try {
      let dest = "/";
      if (mustChange) {
        const changed = await api.changePassword(
          username,
          password,
          newPassword,
          redirect,
        );
        dest = changed.redirect || "/";
        toast.success("Password changed");
      } else {
        const result = await api.passwordLogin(username, password, redirect);
        dest = result.redirect || "/";
        if (result.must_change_password) {
          // Not a failure, so not an error toast: the credential was correct and
          // the next step is the point.
          setMustChange(
            result.reason ?? "Choose a new password to finish signing in.",
          );
          setPending(false);
          return;
        }
      }
      // The session cookie is set; leave the SPA's client-side router out of it
      // so every provider lands the same way.
      // The server's validated destination, never the raw ?redirect=.
      window.location.href = dest;
    } catch (err) {
      setFailure(err instanceof Error ? err.message : "Couldn’t sign you in.");
      setPending(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-3">
      {mustChange && (
        <Notice icon={<KeyRound className="h-4 w-4 shrink-0" />}>
          {mustChange}
        </Notice>
      )}

      <div className="space-y-1.5">
        <Label htmlFor="username" className="text-xs">
          {displayName} username
        </Label>
        <Input
          id="username"
          autoComplete="username"
          autoCapitalize="none"
          spellCheck={false}
          required
          disabled={pending || !!mustChange}
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="password" className="text-xs">
          {mustChange ? "Current password" : "Password"}
        </Label>
        <Input
          id="password"
          type="password"
          autoComplete="current-password"
          required
          disabled={pending || !!mustChange}
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
      </div>

      {mustChange && (
        <>
          <div className="space-y-1.5">
            <Label htmlFor="new-password" className="text-xs">
              New password
            </Label>
            <Input
              id="new-password"
              type="password"
              autoComplete="new-password"
              required
              minLength={8}
              disabled={pending}
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
            />
            <FieldHelp>At least 8 characters.</FieldHelp>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="confirm-password" className="text-xs">
              Confirm new password
            </Label>
            <Input
              id="confirm-password"
              type="password"
              autoComplete="new-password"
              required
              disabled={pending}
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
            />
          </div>
        </>
      )}

      {failure && (
        <Notice
          live
          tone="danger"
          icon={<ShieldAlert className="h-4 w-4 shrink-0" />}
        >
          {failure}
        </Notice>
      )}

      <Button
        type="submit"
        className="w-full gap-2"
        size="lg"
        disabled={pending}
      >
        {pending && <Loader2 className="h-4 w-4 animate-spin" />}
        {pending
          ? mustChange
            ? "Setting your password…"
            : "Signing in…"
          : mustChange
            ? "Set password and sign in"
            : "Sign in"}
      </Button>
    </form>
  );
}

function GitHubMark() {
  return (
    <svg className="h-4 w-4" viewBox="0 0 24 24" fill="currentColor">
      <path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z" />
    </svg>
  );
}
