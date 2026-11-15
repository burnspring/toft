import { useQuery } from "@tanstack/react-query";

import { api } from "@/lib/api";
import { adminGroups, type AdminNavItem } from "@/lib/admin-nav";

/**
 * The admin nav as this install has it: an item that needs an optional
 * capability is absent until the install answers that it has one — absent,
 * not a page that explains itself away. Not-answered-yet renders nothing, so
 * the item cannot appear and then vanish.
 *
 * Storage is read off `/me/capabilities`, whose `storage_available` is the test
 * every storage route's 404 is — never by requesting the inventory and reading
 * its 404, which put a failed request on every admin page of an install
 * without storage.
 */
export function useAdminGroups(enabled = true) {
  const { data: caps } = useQuery({
    queryKey: ["capabilities"],
    queryFn: api.getCapabilities,
    enabled,
  });
  const has = (item: AdminNavItem) =>
    item.capability === "storage" ? caps?.install?.storage_available === true : true;
  return adminGroups
    .map((g) => ({ ...g, items: g.items.filter(has) }))
    .filter((g) => g.items.length > 0);
}
