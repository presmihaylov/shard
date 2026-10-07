// The host ports one sandbox forwards to ports on its own 127.0.0.1.
import { listed } from "./pages.js";
import { port, type Port } from "./records.js";
import type { Transport } from "./transport.js";

/** Ports are the forwards of one sandbox; a running sandbox listens at once, any other from its next start. */
export class Ports {
  constructor(
    private readonly transport: Transport,
    private readonly sandboxId: string,
  ) {}

  /** add forwards hostPort to guestPort, or changes the forward hostPort already has; public listens on every interface of the host, not only 127.0.0.1. */
  async add(hostPort: number, guestPort: number, options: { public?: boolean } = {}): Promise<Port> {
    const body = { guest_port: guestPort, public: options.public || undefined };
    const { data } = await this.transport.api.PUT("/v0/sandboxes/{id}/ports/{host_port}", { params: this.params(hostPort), body });

    return port(data);
  }

  /** list returns the sandbox's forwards in host port order, each as the host serves it now. */
  async list(): Promise<Port[]> {
    const path = { id: this.sandboxId };
    const route = "/v0/sandboxes/{id}/ports";
    const { rows } = await listed(route, "ports", (cursor) => this.transport.api.GET(route, { params: { path, query: { cursor } } }));

    return rows.map(port);
  }

  /** remove ends the forward on hostPort, and the connections it carries. */
  async remove(hostPort: number): Promise<void> {
    await this.transport.api.DELETE("/v0/sandboxes/{id}/ports/{host_port}", { params: this.params(hostPort) });
  }

  private params(hostPort: number): { path: { id: string; host_port: number } } {
    return { path: { id: this.sandboxId, host_port: hostPort } };
  }
}
