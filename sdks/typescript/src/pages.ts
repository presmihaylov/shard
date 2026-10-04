// Every list the daemon answers is paged: rows under one key, and next as the cursor of the page after or null.
import { ProtocolError, isObject } from "./errors.js";

/** listed answers every row of a paged list; page fetches one page from the cursor, undefined for the first. */
export async function listed(route: string, key: string, page: (cursor: string | undefined) => Promise<{ data?: unknown }>): Promise<{ rows: unknown[]; warnings: string[] }> {
  const rows: unknown[] = [];
  const warnings = new Set<string>();
  let cursor: string | undefined;
  do {
    const { data } = await page(cursor);
    const found = isObject(data) ? data[key] : undefined;
    const next = isObject(data) ? data.next : undefined;
    if (!Array.isArray(found) || (next !== null && typeof next !== "string")) {
      throw new ProtocolError(`GET ${route} answered a page with no ${key} or next`);
    }
    const lines: unknown = isObject(data) && data.warnings !== undefined ? data.warnings : [];
    if (!Array.isArray(lines) || !lines.every((line: unknown): line is string => typeof line === "string")) {
      throw new ProtocolError(`GET ${route} answered list warnings that are not strings`);
    }
    rows.push(...found);
    for (const line of lines) {
      warnings.add(line);
    }
    cursor = next || undefined;
  } while (cursor);

  return { rows, warnings: [...warnings] };
}
