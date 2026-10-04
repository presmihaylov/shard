// Run commands in a fresh sandbox, put a file in it, and wait on an app.
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
  } finally {
    await sandbox.remove({ force: true });
  }

  const app = await shard.run(image, "for i in 1 2 3; do echo tick $i; done");
  try {
    // The app ends, but its sandbox stays until it is removed.
    console.log(`app exited ${(await app.wait()).exitCode}`);
    process.stdout.write(await app.logs());
  } finally {
    await app.sandbox.remove({ force: true });
  }
} finally {
  shard.close();
}
