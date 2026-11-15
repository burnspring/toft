import type { SignInClient } from "@/lib/api";

/**
 * What the approval page says, by the program asking (ADR §42, §102).
 *
 * One page decides both `toft login` and RStudio's Add account, because both end
 * in the same credential: a personal key that acts as the person approving. The
 * warning is the mitigation for a code somebody else started, so it has to name
 * the act the reader would have just performed — a command for one, a dialog
 * for the other — or it asks them to check something they cannot.
 */
export interface SignInCopy {
  title: string;
  /** The program, as the Client row names it before the version string. */
  clientLabel: string;
  /** Whether the client reports a hostname at all. rsconnect never does, and a
   * row reading "not reported" on every RStudio sign-in is not a fact worth a
   * glance; for `toft` its absence is the finding. */
  reportsHostname: boolean;
  warningTitle: string;
  /** The act the reader should have just performed, for "If you did not…". */
  act: string;
  expired: string;
  approvedToast: string;
  deniedToast: string;
  approved: string;
  denied: string;
}

const COPY: Record<SignInClient, SignInCopy> = {
  toft: {
    title: "Sign in a terminal",
    clientLabel: "toft",
    reportsHostname: true,
    warningTitle: "Only approve a terminal you are sitting at",
    act: "run toft login there",
    expired: "This sign-in expired. Run toft login again on that machine.",
    approvedToast: "Approved — your terminal is signed in",
    deniedToast: "Denied — that terminal got nothing",
    approved: "Your terminal now has a key and can carry on.",
    denied: "That terminal got nothing.",
  },
  rsconnect: {
    title: "Sign in RStudio",
    clientLabel: "",
    reportsHostname: false,
    warningTitle: "Only approve RStudio you are sitting at",
    act: "add this server as an account in RStudio",
    expired: "This sign-in expired. Add the account again in RStudio.",
    approvedToast: "Approved — RStudio is signed in",
    deniedToast: "Denied — RStudio got nothing",
    approved: "RStudio now has a key and finishes adding the account on its own.",
    denied: "RStudio got nothing.",
  },
};

/** The copy for a client, falling back to `toft`'s for a word this page has not
 * heard of — the server's column default, and the older of the two. */
export function signInCopy(client: string | undefined): SignInCopy {
  return COPY[(client as SignInClient) in COPY ? (client as SignInClient) : "toft"];
}
