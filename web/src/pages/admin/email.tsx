import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Send } from "lucide-react";

import { api, type EmailSettings, type UpdateEmailSettings } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { useHashScroll } from "@/hooks/use-hash-scroll";
import { QueryError } from "@/components/query-error";
import { UnreadableCredentialNotice } from "@/components/unreadable-credential";
import { credentialPlaceholder } from "@/lib/credentials";
import { Section } from "@/components/section";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";
import { Notice } from "@/components/notice";
import { FieldHelp } from "@/components/field-help";
import { FieldRow } from "@/components/field-row";
import { PendingBar } from "@/components/pending-bar";
import { useUnloadGuard } from "@/hooks/use-unload-guard";
import { ADMIN_ONLY_FALLBACK } from "@/lib/query-copy";

const SECURITY_LABEL: Record<EmailSettings["security"], string> = {
  starttls: "STARTTLS (usually port 587)",
  implicit_tls: "Implicit TLS (usually port 465)",
  none: "None — unencrypted",
};

// SMTP leads: it is what a corporate relay speaks, and the API transport is the
// answer to a specific problem — outbound 587 closed at a firewall — rather than
// the better default.
const TRANSPORT_LABEL: Record<EmailSettings["transport"], string> = {
  smtp: "SMTP relay",
  api: "Sending API over HTTPS",
};

const PROVIDER_LABEL: Record<EmailSettings["api_provider"], string> = {
  resend: "Resend",
};

/**
 * Admin → Email.
 *
 * Its own page rather than a card on Settings for two reasons: it holds a
 * credential, and it has an action — the test send — that talks to something
 * outside this install and can fail in ways only its own error text explains.
 *
 * Everything here is optional. With the switch off the platform still records
 * and shows every request it would have announced; it just says nothing out of
 * band, and the banner at the top says so rather than leaving an admin to
 * wonder what stopped working.
 */
export function AdminEmailPage() {
  const queryClient = useQueryClient();
  const { data: settings, isLoading, error, refetch } = useQuery({
    queryKey: ["email-settings"],
    queryFn: api.getEmailSettings,
    retry: false,
  });
  // A setting here is reachable by name from ⌘K, which arrives as
  // `/admin/email#transport` — and the card it names only exists once the
  // form has something to render.
  useHashScroll(!!settings);

  if (error) {
    return (
      <>
        <PageHeader title="Email" />
        <QueryError
          title="The mail settings could not be read"
          error={error}
          fallback={ADMIN_ONLY_FALLBACK}
          onRetry={() => void refetch()}
        />
      </>
    );
  }

  if (isLoading || !settings) {
    return (
      <>
        <PageHeader title="Email" />
        <div className="space-y-6">
          <Skeleton className="h-40 w-full rounded-lg" />
          <Skeleton className="h-64 w-full rounded-lg" />
        </div>
      </>
    );
  }

  return (
    <>
      <PageHeader title="Email" />
      {/* Keyed on what the server stored, so a save re-seeds the form from the
          saved document rather than leaving edited state to drift from it. */}
      {/* The measure both settings surfaces use; uncapped, inputs ran ~1,125px. */}
      <div className="max-w-3xl">
        <EmailSettingsForm
          key={`${settings.transport}:${settings.host}:${settings.port}:${settings.password_set}:${settings.api_key_set}:${settings.password_unreadable}:${settings.api_key_unreadable}`}
          settings={settings}
          onSaved={(saved) => {
            queryClient.setQueryData(["email-settings"], saved);
            queryClient.invalidateQueries({ queryKey: ["email-settings"] });
          }}
        />
      </div>
    </>
  );
}

function EmailSettingsForm({
  settings,
  onSaved,
}: {
  settings: EmailSettings;
  onSaved: (saved: EmailSettings) => void;
}) {
  const [enabled, setEnabled] = useState(settings.enabled);
  const [transport, setTransport] = useState<EmailSettings["transport"]>(settings.transport);
  const [host, setHost] = useState(settings.host);
  const [port, setPort] = useState(String(settings.port));
  const [security, setSecurity] = useState<EmailSettings["security"]>(settings.security);
  const [username, setUsername] = useState(settings.username);
  // Never seeded from the server: neither credential comes back as a value, and
  // an input that shows dots it did not receive is a lie.
  const [password, setPassword] = useState("");
  const [apiProvider, setApiProvider] = useState<EmailSettings["api_provider"]>(
    settings.api_provider,
  );
  const [apiKey, setApiKey] = useState("");
  const [fromAddress, setFromAddress] = useState(settings.from_address);
  const [fromName, setFromName] = useState(settings.from_name);
  // The install's name, offered for an empty From name (ADR: the sender
  // display name stays its own setting). It fills the draft and saves nothing:
  // copying it automatically would change a header relays and inbox rules may
  // match on.
  const { data: platform } = useQuery({
    queryKey: ["platform-settings"],
    queryFn: api.getPlatformSettings,
    retry: false,
  });
  const installName = platform?.install_name?.trim() ?? "";
  const [replyTo, setReplyTo] = useState(settings.reply_to);
  const [skipVerify, setSkipVerify] = useState(settings.skip_verify);
  const [notifyAdmin, setNotifyAdmin] = useState(settings.notify_admin);
  const [notifyApp, setNotifyApp] = useState(settings.notify_app);
  const [adminGroupAddress, setAdminGroupAddress] = useState(settings.admin_group_address);

  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testError, setTestError] = useState("");

  const changed = [
    enabled !== settings.enabled,
    transport !== settings.transport,
    host !== settings.host,
    port !== String(settings.port),
    security !== settings.security,
    username !== settings.username,
    password !== "",
    apiProvider !== settings.api_provider,
    apiKey !== "",
    fromAddress !== settings.from_address,
    fromName !== settings.from_name,
    replyTo !== settings.reply_to,
    skipVerify !== settings.skip_verify,
    notifyAdmin !== settings.notify_admin,
    notifyApp !== settings.notify_app,
    adminGroupAddress !== settings.admin_group_address,
  ].filter(Boolean).length;
  const dirty = changed > 0;
  // Three sections and one Save: without these, an edit to the relay host at
  // the top was lost silently on the way to another page.
  useUnloadGuard(dirty, "The email settings have unsaved changes");

  function discard() {
    setEnabled(settings.enabled);
    setTransport(settings.transport);
    setHost(settings.host);
    setPort(String(settings.port));
    setSecurity(settings.security);
    setUsername(settings.username);
    setPassword("");
    setApiProvider(settings.api_provider);
    setApiKey("");
    setFromAddress(settings.from_address);
    setFromName(settings.from_name);
    setReplyTo(settings.reply_to);
    setSkipVerify(settings.skip_verify);
    setNotifyAdmin(settings.notify_admin);
    setNotifyApp(settings.notify_app);
    setAdminGroupAddress(settings.admin_group_address);
  }

  async function save() {
    setSaving(true);
    setTestError("");
    const payload: UpdateEmailSettings = {
      enabled,
      transport,
      host: host.trim(),
      port: Number(port) || 0,
      security,
      username: username.trim(),
      api_provider: apiProvider,
      from_address: fromAddress.trim(),
      from_name: fromName.trim(),
      reply_to: replyTo.trim(),
      skip_verify: skipVerify,
      notify_admin: notifyAdmin,
      notify_app: notifyApp,
      // Sent even when blank, unlike the credentials: clearing this is a state
      // an administrator has to be able to get back to, and empty means
      // "notify each administrator individually" rather than "keep what is
      // stored".
      admin_group_address: adminGroupAddress.trim(),
    };
    // An empty credential means "keep the stored one" on the way through the
    // API, so it is omitted rather than sent blank.
    if (password.trim()) payload.password = password;
    if (apiKey.trim()) payload.api_key = apiKey.trim();
    try {
      const saved = await api.updateEmailSettings(payload);
      onSaved(saved);
      setPassword("");
      setApiKey("");
      // "Saved"; the one consequence worth a warning is every notification
      // off while email is on, which the reader may not have meant.
      if (enabled && !notifyAdmin && !notifyApp) {
        toast.warning("Saved — every notification is switched off");
      } else {
        toast.success("Saved");
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t save the email settings");
    } finally {
      setSaving(false);
    }
  }

  async function sendTest() {
    setTesting(true);
    setTestError("");
    try {
      const res = await api.sendTestEmail();
      toast.success(`Test email sent to ${res.sent_to}`);
    } catch (err) {
      const message = err instanceof Error ? err.message : "The test send failed";
      setTestError(message);
      toast.error(message);
    } finally {
      setTesting(false);
    }
  }

  return (
    <div className="space-y-6">
      {!settings.enabled && (
        <Notice tone="neutral" size="md" title="Email is off">
          Nothing is emailed. Publisher requests still show on the Publisher requests page.
        </Notice>
      )}

      <Section
        id="transport"
        title="Delivery"
      >
        {/* One row per setting, label left and control right — the shape
            Admin → Settings and the app's Settings tab use (`FieldRow`). */}
        <div className="divide-y divide-border/60 px-4 [&>*]:py-4">
          <FieldRow
            label="Send email"
            htmlFor="email-enabled"
            help="When off, no notification is sent and nothing fails."
          >
            <Switch id="email-enabled" checked={enabled} onCheckedChange={setEnabled} />
          </FieldRow>

          <FieldRow
            label="Transport"
            htmlFor="email-transport"
            help={
              transport === "api"
                ? "Sends over HTTPS on port 443. Use this when outbound SMTP is blocked."
                : "The usual choice: your own mail relay."
            }
          >
            <Select
              value={transport}
              onValueChange={(v) => setTransport((v ?? "smtp") as EmailSettings["transport"])}
            >
              <SelectTrigger id="email-transport" className="w-full" aria-label="Transport">
                <SelectValue>
                  {(v) => (v ? TRANSPORT_LABEL[v as EmailSettings["transport"]] : "Choose…")}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="smtp">{TRANSPORT_LABEL.smtp}</SelectItem>
                <SelectItem value="api">{TRANSPORT_LABEL.api}</SelectItem>
              </SelectContent>
            </Select>
          </FieldRow>

          {transport === "api" ? (
            <>
              <FieldRow label="Provider" htmlFor="email-provider">
                <Select
                  value={apiProvider}
                  onValueChange={(v) =>
                    setApiProvider((v ?? "resend") as EmailSettings["api_provider"])
                  }
                >
                  <SelectTrigger id="email-provider" className="w-full" aria-label="Provider">
                    <SelectValue>
                      {(v) => (v ? PROVIDER_LABEL[v as EmailSettings["api_provider"]] : "Choose…")}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="resend">{PROVIDER_LABEL.resend}</SelectItem>
                  </SelectContent>
                </Select>
              </FieldRow>

              <FieldRow
                label="Sending API key"
                htmlFor="email-api-key"
                help="Resend only sends from a domain you have verified with them."
                extra={
                  settings.api_key_unreadable ? (
                    <UnreadableCredentialNotice what="API key" />
                  ) : (
                    <FieldHelp>
                      {settings.api_key_set
                        ? "Leave blank to keep the stored one. It is encrypted and never shown again."
                        : "Encrypted and never shown again. A key with send permission is enough."}
                    </FieldHelp>
                  )
                }
              >
                <Input
                  id="email-api-key"
                  type="password"
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  placeholder={credentialPlaceholder(
                    settings.api_key_set,
                    settings.api_key_unreadable,
                    "re_…",
                  )}
                  autoComplete="new-password"
                  className="font-mono text-xs md:text-xs"
                />
              </FieldRow>
            </>
          ) : (
            <>
              <FieldRow label="SMTP host" htmlFor="email-host">
                <Input
                  id="email-host"
                  value={host}
                  onChange={(e) => setHost(e.target.value)}
                  placeholder="smtp.example.com"
                />
              </FieldRow>
              <FieldRow label="SMTP port" htmlFor="email-port">
                <Input
                  id="email-port"
                  value={port}
                  inputMode="numeric"
                  onChange={(e) => setPort(e.target.value.replace(/[^0-9]/g, ""))}
                  placeholder="587"
                  className="w-24"
                />
              </FieldRow>

              <FieldRow label="Transport security" htmlFor="email-security">
                <Select
                  value={security}
                  onValueChange={(v) => setSecurity((v ?? "starttls") as EmailSettings["security"])}
                >
                  <SelectTrigger id="email-security" className="w-full" aria-label="Transport security">
                    {/* base-ui's Value renders the raw value, not the chosen item's
                        text — format it here. */}
                    <SelectValue>
                      {(v) => (v ? SECURITY_LABEL[v as EmailSettings["security"]] : "Choose…")}
                    </SelectValue>
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="starttls">{SECURITY_LABEL.starttls}</SelectItem>
                    <SelectItem value="implicit_tls">{SECURITY_LABEL.implicit_tls}</SelectItem>
                    <SelectItem value="none">{SECURITY_LABEL.none}</SelectItem>
                  </SelectContent>
                </Select>
              </FieldRow>

              <FieldRow
                label="SMTP username"
                htmlFor="email-username"
                help="Leave empty if the relay needs no sign-in."
              >
                <Input
                  id="email-username"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  autoComplete="off"
                />
              </FieldRow>
              <FieldRow
                label="SMTP password"
                htmlFor="email-password"
                extra={
                  settings.password_unreadable ? (
                    <UnreadableCredentialNotice what="password" />
                  ) : (
                    settings.password_set && (
                      <FieldHelp>
                        Leave blank to keep the stored one. It is encrypted and never shown again.
                      </FieldHelp>
                    )
                  )
                }
              >
                <Input
                  id="email-password"
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  placeholder={credentialPlaceholder(
                    settings.password_set,
                    settings.password_unreadable,
                    "",
                  )}
                  autoComplete="new-password"
                  className="font-mono text-xs md:text-xs"
                />
              </FieldRow>

              <FieldRow
                label="Accept a self-signed certificate"
                htmlFor="email-skip-verify"
                help="Turns off certificate checks. Anyone who can intercept the connection can read the credential above."
              >
                <Switch
                  id="email-skip-verify"
                  checked={skipVerify}
                  onCheckedChange={setSkipVerify}
                />
              </FieldRow>
            </>
          )}
        </div>
      <div className="flex flex-wrap items-center gap-3 border-t border-border/60 px-4 py-3">
        <Button
          variant="outline"
          onClick={sendTest}
          disabled={testing || saving || dirty || !settings.enabled || !settings.test_recipient}
        >
          <Send className="mr-1.5 h-3.5 w-3.5" />
          {testing ? "Sending…" : "Send test email"}
        </Button>
        <p className="text-xs text-muted-foreground">
          {dirty
            ? "Save first — the test uses the saved settings."
            : !settings.enabled
              ? "Turn email on and save to send a test."
              : settings.test_recipient
                ? `Sends to your own address, ${settings.test_recipient}.`
                : "Your account has no email address, so there is nowhere to send a test."}
        </p>
      </div>
      </Section>

      <Section
        id="identity"
        title="Sender"
        description="Most relays refuse a From address on a domain they don’t handle."
      >
        <div className="divide-y divide-border/60 px-4 [&>*]:py-4">
          <FieldRow label="From address" htmlFor="email-from">
            <Input
              id="email-from"
              value={fromAddress}
              onChange={(e) => setFromAddress(e.target.value)}
              placeholder="toft@example.com"
            />
          </FieldRow>
          <FieldRow
            label="From name"
            htmlFor="email-from-name"
            help={
              installName !== "" && fromName.trim() === "" ? (
                <>
                  Empty sends a bare address.{" "}
                  <Button
                    type="button"
                    variant="link"
                    size="sm"
                    className="h-auto p-0 text-xs"
                    onClick={() => setFromName(installName)}
                  >
                    Use “{installName}”
                  </Button>
                </>
              ) : undefined
            }
          >
            <Input
              id="email-from-name"
              value={fromName}
              onChange={(e) => setFromName(e.target.value)}
              placeholder="Toft"
            />
          </FieldRow>
          <FieldRow
            label="Reply-to (optional)"
            htmlFor="email-reply-to"
            help="Where a reply lands. Empty means replies go to the From address."
          >
            <Input
              id="email-reply-to"
              value={replyTo}
              onChange={(e) => setReplyTo(e.target.value)}
              placeholder="platform-team@example.com"
            />
          </FieldRow>
        </div>
      </Section>

      <Section
        id="audiences"
        title="Notifications"
        description="Switch off an audience to stop mailing it. Everything stays visible here."
      >
        <div className="divide-y divide-border/60 px-4 [&>*]:py-4">
          <FieldRow
            label="Administrator notifications"
            htmlFor="email-notify-admin"
            help="Mail to administrators, such as a waiting publisher request."
          >
            <Switch
              id="email-notify-admin"
              checked={notifyAdmin}
              onCheckedChange={setNotifyAdmin}
            />
          </FieldRow>

          <FieldRow
            label="Administrator notification address"
            htmlFor="email-admin-group"
            help="A list to send to instead of each administrator. Leave empty to mail every administrator with an address on file."
            extra={
              !notifyAdmin && adminGroupAddress.trim() !== "" && (
                <FieldHelp>
                  Administrator notifications are off above, so nothing is sent here.
                </FieldHelp>
              )
            }
          >
            <Input
              id="email-admin-group"
              type="email"
              value={adminGroupAddress}
              onChange={(e) => setAdminGroupAddress(e.target.value)}
              placeholder="toft-admins@example.com"
            />
          </FieldRow>

          <FieldRow
            label="App and access notifications"
            htmlFor="email-notify-app"
            help="Mail to people about their own apps and access, such as an approved request."
            extra={
              enabled && !notifyAdmin && !notifyApp && (
                <FieldHelp>Both are off, so only the test email is sent.</FieldHelp>
              )
            }
          >
            <Switch id="email-notify-app" checked={notifyApp} onCheckedChange={setNotifyApp} />
          </FieldRow>
        </div>
      </Section>

      {/* The save is the sticky bar, as on Settings: a lone Save below three
          sections was off screen while somebody edited the first one. */}
      <PendingBar
        count={changed}
        broken={0}
        saving={saving}
        onDiscard={discard}
        onSave={() => void save()}
      />

      {testError && (
        <Notice
          live
          tone="danger"
          size="md"
          title={
            settings.transport === "api"
              ? `${PROVIDER_LABEL[settings.api_provider]} refused the test send`
              : "The relay refused the test send"
          }
        >
          <p className="font-mono whitespace-pre-wrap">{testError}</p>
        </Notice>
      )}
    </div>
  );
}
