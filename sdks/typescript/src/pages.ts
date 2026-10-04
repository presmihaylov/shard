// Every list the daemon answers is paged: rows under one key, and next as the cursor of the page after or null.
import { ProtocolError, isObject } from "./errors.js";
import type { Transport } from "./transport.js";

/** listed answers every row of a paged list, one page after another. */
export async function listed(transport: Transport, route: string, key: string, query: Record<string, string> = {}): Promise<unknown[]> {
  const rows: unknown[] = [];
  let cursor = "";
  do {
    const page = await transport.call("GET", route, { query: cursor ? { ...query, cursor } : query });
    const found = isObject(page) ? page[key] : undefined;
    const next = isObject(page) ? page.next : undefined;
    if (!Array.isArray(found) || (next !== null && typeof next !== "string")) {
      throw new ProtocolError(`GET ${route} answered a page with no ${key} or next`);
    }
    rows.push(...found);
    cursor = next ?? "";
  } while (cursor);

  return rows;
}
