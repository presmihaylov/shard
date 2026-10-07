// Run commands in a fresh sandbox, put a file in it, and wait on a supervised process.
import { Shard } from "useshards";

const image = "docker.io/library/alpine:3";

const shard = new Shard();
try {
  const { version } = await shard.version();
  const { fork } = await shard.capabilities();
  console.log(`daemon ${version}, fork ${fork ? "supported" : "unsupported"}`);

  const sandbox = await shard.create({ image });
  try {
    const uname = await sandbox.exec(["uname", "-sr"]);
    console.log(`uname exited ${uname.exitCode}: ${uname.stdout.trim()}`);

    await sandbox.files.write("/tmp/hello.txt", "hello from the host\n");
    process.stdout.write((await sandbox.exec("tr a-z A-Z < /tmp/hello.txt")).stdout);

    // A background command runs on while this client does other work.
    const command = await sandbox.exec("sleep 1; echo done", { background: true });
    console.log(`background command ${command.id} said ${(await command.wait()).stdout.trim()}`);

    // A process ends, but its sandbox runs on until it is stopped or removed.
    const ticker = await sandbox.run("for i in 1 2 3; do echo tick $i; done", { name: "ticker", restart: { policy: "no" } });
    console.log(`process ${ticker.name} exited ${(await ticker.wait()).status.exit?.exitCode}`);
    process.stdout.write(await ticker.logs());
  } finally {
    await sandbox.remove({ force: true });
  }
} finally {
  shard.close();
}
