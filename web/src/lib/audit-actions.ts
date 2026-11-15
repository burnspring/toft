import type { ActivityEntry } from "@/lib/api";

/**
 * What an audited action *is*, in a sentence rather than an identifier.
 *
 * The Action column already shows the raw string — it is what someone greps
 * for — so this column has to earn its place by saying something else. Only the
 * actions the backend actually writes are listed; anything new falls through to
 * `humaniseAction`, which is dull but never wrong.
 */
export const ACTION_PHRASE: Record<string, string> = {
  "app.create": "An app was created",
  "app.delete": "An app was deleted",
  "app.visibility_change": "Who can open this app changed",
  // Its own sentence rather than folded into the line above it. That one is
  // who may *open* the app; this one is who may know it is there — and
  // somebody reading this page after a disclosure is looking for exactly the
  // second, which a shared phrase would make them open every row to find.
  "app.listable_change": "Who can see that this app exists changed",
  "app.retention_change": "How many deployments this app keeps changed",
  "app.never_sleep_change": "Whether the app may sleep when idle changed",
  "app.hostname_change": "The app’s own hostname changed",
  "app.viewer_identity.change": "Whether the app can see who is viewing it changed",
  "app.frame_ancestors.change": "Which sites may embed this app changed",
  "app.garth.unlink": "Unlinked from Garth",
  "app.lock": "An administrator locked the app",
  // Not "an administrator": nobody was present. The dependency policy locked
  // it on a scheduled pass, and a line that named a person would be wrong about
  // the only thing this log exists to record.
  "app.lock_dependency": "The dependency policy locked the app",
  "app.unlock": "An administrator unlocked the app",
  // The other half of the policy's lock: a deploy whose release no longer
  // carried the package lifted it. Nobody unlocked it either.
  "app.unlock_dependency": "A clean deploy lifted the dependency lock",
  "app.start": "The app was started",
  "app.restart": "The app was restarted",
  "app.restart_schedule.pause": "The scheduled restart was paused until the next deploy",
  "app.restart_schedule.resume": "The scheduled restart was resumed",
  "app.render_schedule.pause": "The re-render schedule was paused until the next deploy",
  "app.render_schedule.resume": "The re-render schedule was resumed",
  "app.stop": "The app was stopped",
  "app.notify_failures_change": "Failure email for this app was switched on or off",
  "app.release_deleted": "A deployment was deleted",
  "app.release_promoted": "An earlier deployment was made current",
  "app.deploy": "A deployment was started",
  "app.git_token_set": "The repository credential was set",
  "app.git_token_cleared": "The repository credential was cleared",
  "app.env_change": "The app’s environment variables changed",
  "app.tag_set": "A tag value was set",
  "app.tag_clear": "A tag value was cleared",
  "object_grant.create": "Someone was given access",
  "object_grant.modify": "Someone’s access was changed",
  "access_request.create": "Access was requested",
  "access_request.approve": "An access request was approved",
  "access_request.deny": "An access request was denied",
  "access_request.withdraw": "An access request was withdrawn",
  "user.role_change": "A system role changed",
  "user.lock": "An account was locked",
  "user.unlock": "An account was unlocked",
  "user.scanner_set": "An account was marked as the scanner",
  "user.scanner_cleared": "An account stopped being the scanner",
  "user.activate": "An account was activated",
  "user.delete": "An account was deleted",
  "password.set": "A password was set",
  "session.signin": "Signed in",
  "session.signin_failed": "A sign-in was refused",
  "session.signout": "Signed out",
  "group.create": "A group was created",
  "group.delete": "A group was deleted",
  "auth_provider.create": "A sign-in provider was added",
  "auth_provider.update": "A sign-in provider was changed",
  "auth_provider.delete": "A sign-in provider was removed",
  "git_connection.created": "A git host was connected",
  "git_connection.updated": "A connected git host was changed",
  "git_connection.disabled": "A connected git host was disabled",
  "git_connection.deleted": "A connected git host was removed",
  "git_connection.webhook_secret_rotated": "A connected git host’s webhook secret was replaced",
  "git_connection.webhook_enabled": "A connected git host’s webhook was set up",
  "git_connection.webhook_disabled": "A connected git host’s webhook was turned off",
  "platform_settings.update": "Platform settings were changed",
  "announcement.set": "The announcement banner was published",
  "announcement.clear": "The announcement banner was cleared",
  "announcement_email.create": "An announcement was drafted",
  "announcement_email.update": "A draft announcement was edited",
  "announcement_email.test": "A test copy of an announcement was sent",
  "announcement_email.send": "An announcement was sent to the install",
  "announcement_email.delete": "An announcement was deleted",
  "knowledge_base.set": "The knowledge base was pointed at an app",
  "knowledge_base.clear": "The knowledge base was cleared",
  "email_settings.update": "Email settings were changed",
  "tag.create": "A tag was created",
  "tag.update": "A tag was changed",
  "tag.delete": "A tag was deleted",
  "storage_resource.create": "A storage resource was created",
  "storage_drive.label": "A storage drive’s label was changed",
  "storage_resource.class_recorded": "A volume’s storage class was recorded",
  "storage_resource.repoint": "A storage resource was re-pointed to another drive",
  "storage_resource.delete": "A storage resource was deleted",
  "storage_resource.destroy_started": "A deleted storage resource’s data was handed to the cluster to remove",
  "storage_resource.destroyed": "A deleted storage resource’s data finished being removed",
  "storage_resource.name_released": "A name held by a deleted volume’s kept files was released",
  "storage_mapping.create": "Storage was attached to an app",
  "storage_mapping.update": "An app’s storage attachment was changed",
  "storage_mapping.delete": "Storage was detached from an app",
  "storage_manager.put": "Someone was made a delegate for storage",
  "storage_manager.delete": "A storage delegate was revoked",
  "storage_default_manager.put": "Someone was made a default delegate for new storage",
  "storage_default_manager.delete": "A default storage delegate was removed",
  "storage.level_promoted": "A storage level was moved to Read & write when Can replace was retired",
  "job.pause": "A job’s schedule was paused or resumed",
  "app.create_refused_tier": "Creating an app was refused by the license",
  "app.data_reset": "The app’s saved data was cleared",
  "app.execution_mode.change": "Where the app runs changed",
  "app.memory_limit.change": "How much memory the app may use changed",
  "app.cpu_limit.change": "How much CPU the app may use changed",
  "job.resources.change": "A job’s memory or CPU limit changed",
  "app.transfer_ownership": "The app was handed to a new owner",
  "user.api_key_create": "A personal API key was created",
  "user.lock_dormant": "An unused account was locked automatically",
  "deploy_key.create": "A deploy key was created",
  "trusted_issuer.create": "An issuer was trusted to vouch for people",
  "trusted_issuer.update": "A trusted issuer changed",
  "trusted_issuer.delete": "An issuer stopped being trusted",
  "exchanged_key.create": "An identity token was exchanged for a short-lived key",
  "exchanged_key.revoke": "A short-lived key was revoked",
  "group.update": "A group was renamed",
  "group.member_add": "Someone joined a group",
  "group.member_remove": "Someone was removed from a group",
  "group.claim_map": "A group was filled from a directory",
  "group.claim_unmap": "A group stopped being filled from a directory",
  "credential.create": "A credential was created",
  "credential.update": "A credential’s values were changed",
  "credential.delete": "A credential was deleted",
  "credential.attach": "An app was given a credential",
  "credential.detach": "A credential was taken away from an app",
  "auth_provider.scim_token_set": "A directory was given a feed into this install",
  "auth_provider.scim_token_revoke": "A directory’s feed was switched off",
  "scim.refused": "A directory was refused a change to an account",
  "group.sync_capped": "Someone is in more directory groups than the limit allows",
  "object_grant.remove": "Someone’s access was removed",
  "store_grant.put": "Someone was given access to a store",
  "store_grant.delete": "Someone’s access to a store was revoked",
  "store.presign": "A signed link to a file was issued",
  "store.link": "An app made a link to a file to send someone",
  "store.link.refused": "Someone opened a link to a file they cannot read, or that is not there",
  "storage_resource.rename": "A storage resource’s description was changed",
  "gallery.deploy": "An app was deployed from the gallery",
  "gallery.update": "A gallery app was moved to a new version",
  "gallery.detach": "An app stopped following the gallery",
  "job_run.trigger": "A job was started by hand",
  "job_run.cancel": "A job run was cancelled",
  "job_run.rerun": "A job run was started again",
  "feature.enable": "A feature was switched on or off",
  "integrations.settings": "How long the send log is kept changed",
  "integration.connection.create": "An integration was connected",
  "integration.connection.update": "An integration connection was changed",
  "integration.connection.delete": "An integration connection was removed",
  "integration.place.add": "A Teams team or chat was added to a bot connection",
  "integration.place.remove": "A Teams team or chat was removed from a bot connection",
  "integration.attached": "An integration was attached to an app",
  "integration.updated": "An app’s integration was switched on or off",
  "integration.detached": "An integration was detached from an app",
  "integration.destination": "An app changed where its messages go",
  "telemetry_settings.update": "Telemetry settings were changed",
  "dependency_feed_credential.update": "The advisory feed credential was set",
  "user.api_key_revoke": "A personal API key was revoked",
  "user.cli_login_approve": "A terminal sign-in was approved",
  "user.cli_login_deny": "A terminal sign-in was refused",
  "session.revoke_all": "Every session on an account was ended",
  "admin.user_api_key_revoke": "An administrator revoked someone’s API key",
  "app.git_connection_set": "The app was pointed at a connected git host",
  "app.git_connection_cleared": "The app stopped using a connected git host",
  "auth_provider.enabled": "A sign-in provider was switched on or off",
  "app_source_changed": "The app’s repository was changed",
  "user.api_key_disable": "A personal API key was disabled",
  "user.api_key_enable": "A personal API key was re-enabled",
  "user.api_key_rename": "A personal API key was renamed",
  "admin.user_api_key_disable": "An administrator disabled someone’s API key",
  "admin.user_api_key_enable": "An administrator re-enabled someone’s API key",
  "admin.user_api_key_rename": "An administrator renamed someone’s API key",
  "deploy_key.disable": "A deploy key was disabled",
  "deploy_key.enable": "A deploy key was re-enabled",
  "deploy_key.revoke": "A deploy key was revoked",
  "deploy_key.may_attach_set": "Whether a deploy key may attach storage was changed",
  "identity.linked": "A sign-in was linked to an existing account by its email",
  "identity.provisioned": "An account was created at its first sign-in",
  "build_cache.reclaim": "The build cache was emptied",
  "audit_log.pruned": "Records past the retention period were removed",
  "audit_log.app_scope_started": "Filtering by app starts here; earlier rows name an app only when it was their target",
  "deploy_source_refused": "A deploy was refused by the source policy",
  "source_policy_changed": "The trusted-sources policy was changed",
  "source_pattern_added": "A trusted source was added",
  "source_pattern_updated": "A trusted source was changed",
  "source_pattern_removed": "A trusted source was removed",
  "dependency_waiver_granted": "A dependency finding was waived for this app",
  "dependency_waiver_revoked": "A dependency waiver was revoked",
};

/**
 * The fallback: "app.release_deleted" → "Release deleted".
 *
 * Dull but never wrong, and it should be rare — thirty-three of the actions this
 * backend writes were reaching it, so a column of written sentences was
 * interrupted by *Put*, *Presign*, *Change*, *Member add* and *Api key create*.
 * Title-casing the last segment of an enum is naming a thing by how it is
 * stored, which is the one thing this column exists not to do.
 *
 * `lib/audit-actions.test.ts` walks the phrases against the actions the Go side
 * actually writes, so the next one to be added has to bring its sentence.
 */
function humaniseAction(action: string): string {
  const tail = action.slice(action.lastIndexOf(".") + 1).replace(/_/g, " ");
  return tail.charAt(0).toUpperCase() + tail.slice(1);
}

/**
 * What to show in the Summary column.
 *
 * An event carries its own sentence, written where the failure was noticed. An
 * audit row does not — the backend stores the action as the summary, because
 * copy does not belong in a SQL UNION — so it is phrased here.
 */
export function summaryOf(entry: ActivityEntry): string {
  if (entry.source === "event") return entry.summary;
  return ACTION_PHRASE[entry.action] ?? humaniseAction(entry.action);
}
