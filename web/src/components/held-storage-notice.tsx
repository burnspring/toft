import { Link } from "react-router-dom";
import { Notice } from "@/components/notice";
import type { HeldStorageMapping } from "@/lib/api";

/**
 * Why a user or group was not deleted: its storage delegations attach storage
 * to apps (ADR § Storage somebody else can attach). Each row links the
 * resource's page, where revoking the delegation names and restarts the apps —
 * the delete goes through once nothing is attached.
 */
export function HeldStorageNotice({ subject, mappings }: { subject: string; mappings: HeldStorageMapping[] }) {
  return (
    <Notice tone="danger" title={`${subject} attaches storage to apps`} live>
      <p>Revoke these delegations first. Revoking restarts the apps it names; then delete again.</p>
      <ul className="mt-2 space-y-1">
        {mappings.map((m) => (
          <li key={`${m.resource_id}:${m.app_id}:${m.path}`}>
            <Link to={`/admin/storage/${m.resource_name}`} className="font-mono font-medium hover:underline">
              {m.resource_name}
            </Link>{" "}
            on {m.app_name} <span className="font-mono">{m.path}</span>
          </li>
        ))}
      </ul>
    </Notice>
  );
}
