// The records a fake daemon answers, with every field a test does not care about filled in.

export function sandboxRecord(fields: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "sb_1",
    name: "web",
    image: "docker.io/library/alpine:3.20",
    digest: "sha256:abc",
    provider: "gvisor",
    state: "running",
    resources: { memory_mib: 512, vcpus: 1, disk_mib: 1024 },
    command: [],
    secrets: [],
    created_at: "2026-10-04T10:00:00.123456789Z",
    started_at: "2026-10-04T10:00:01Z",
    ...fields,
  };
}
