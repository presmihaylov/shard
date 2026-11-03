// The sandboxes one sandbox links, each reached by name at one address every linker shares.
import { link, records, type Link } from "./records.js";
import type { Transport } from "./transport.js";

/** Links are the links of one sandbox; a link outlives a stop of either end and goes when its target is removed. */
export class Links {
  constructor(
    private readonly transport: Transport,
    private readonly sandboxId: string,
  ) {}

  /** add links target, a sandbox id or name, or answers the link the sandbox already holds to it. */
  async add(target: string): Promise<Link> {
    const { data } = await this.transport.api.PUT("/v0/sandboxes/{id}/links/{target}", { params: this.params(target) });

    return link(data);
  }

  /** list returns the sandbox's links in the order they were added. */
  async list(): Promise<Link[]> {
    const { data } = await this.transport.api.GET("/v0/sandboxes/{id}/links", { params: { path: { id: this.sandboxId } } });

    return records(data?.links, "links", link);
  }

  /** remove drops the link to target; its address is free again once no sandbox links target. */
  async remove(target: string): Promise<void> {
    await this.transport.api.DELETE("/v0/sandboxes/{id}/links/{target}", { params: this.params(target) });
  }

  private params(target: string): { path: { id: string; target: string } } {
    return { path: { id: this.sandboxId, target } };
  }
}
