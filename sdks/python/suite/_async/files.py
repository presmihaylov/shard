from __future__ import annotations

import os
from pathlib import Path

from useshards import NotFoundError

from .._shared import MiB, equal, local_dir, ok, read_local, remove_local, tree, write_local
from .harness import AsyncContext, Check, rejects


def scratch(ctx: AsyncContext) -> Path:
    """A local temp dir the check's cleanup removes."""
    path = local_dir(ctx.prefix)

    async def remove() -> None:
        remove_local(path)

    ctx.defer(f"remove {path}", remove)
    return path


async def write_read(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    folder = f"/tmp/{ctx.name('files')}"
    await sandbox.files.mkdir(folder)
    await sandbox.files.write(f"{folder}/hello.txt", "hello files\n")
    equal(await sandbox.files.read_text(f"{folder}/hello.txt"), "hello files\n")
    equal((await sandbox.exec(["cat", f"{folder}/hello.txt"])).stdout, "hello files\n", "the guest sees the write")
    data = os.urandom(3 * MiB)
    await sandbox.files.write(f"{folder}/blob", data)
    ok(await sandbox.files.read(f"{folder}/blob") == data, "bytes round-trip unchanged")
    await rejects(NotFoundError, lambda: sandbox.files.read(f"{folder}/missing"))


async def metadata(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    folder = f"/tmp/{ctx.name('meta')}"
    await sandbox.files.mkdir(folder)
    await sandbox.files.write(f"{folder}/script.sh", "#!/bin/sh\necho hi\n", mode=0o750)
    info = await sandbox.files.stat(f"{folder}/script.sh")
    equal(info.type, "file")
    equal(info.size, 18)
    equal(info.mode, 0o750)
    ok(info.mtime.tzinfo is not None, f"mtime {info.mtime} carries its zone")
    equal((await sandbox.exec([f"{folder}/script.sh"])).stdout, "hi\n", "the mode reaches the guest")
    equal((await sandbox.files.stat(folder)).type, "dir")
    await rejects(NotFoundError, lambda: sandbox.files.stat(f"{folder}/missing"))


async def list_mkdir_remove(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    folder = f"/tmp/{ctx.name('tree')}"
    await sandbox.files.mkdir(f"{folder}/a/b", parents=True)
    await sandbox.files.write(f"{folder}/a/one.txt", "1")
    names = sorted(f"{entry.name}:{entry.type}" for entry in await sandbox.files.list(f"{folder}/a"))
    equal(names, ["b:dir", "one.txt:file"])
    await sandbox.files.remove(f"{folder}/a/one.txt")
    await rejects(NotFoundError, lambda: sandbox.files.stat(f"{folder}/a/one.txt"))
    await sandbox.files.remove(folder, recursive=True)
    await rejects(NotFoundError, lambda: sandbox.files.list(folder))


async def file_transfer(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    local = scratch(ctx)
    data = os.urandom(5 * MiB)
    write_local(local / "up.bin", data)
    remote = f"/tmp/{ctx.name('transfer')}.bin"
    await sandbox.files.upload(local / "up.bin", remote)
    equal((await sandbox.files.stat(remote)).size, len(data))
    await sandbox.files.download(remote, local / "down.bin")
    ok(read_local(local / "down.bin") == data, "a file transfer round-trips every byte")


async def dir_transfer(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    local = scratch(ctx)
    source = local / "source"
    write_local(source / "top.txt", b"top\n")
    write_local(source / "nested" / "mid.bin", os.urandom(MiB))
    write_local(source / "nested" / "deeper" / "leaf.txt", b"leaf\n")
    write_local(source / "nested" / "empty", b"")
    remote = f"/tmp/{ctx.name('dir')}"
    await sandbox.files.upload_dir(source, remote)
    equal(await sandbox.files.read_text(f"{remote}/nested/deeper/leaf.txt"), "leaf\n")
    back = local / "back"
    await sandbox.files.download_dir(remote, back)
    want = tree(source)
    got = tree(back)
    equal(sorted(got), sorted(want), "the same files come back")
    for path, data in want.items():
        ok(got.get(path) == data, f"{path} came back changed")
    ok((back / "nested" / "deeper").is_dir(), "the nested directory comes back")


CHECKS = [
    Check("files.write_read", write_read),
    Check("files.metadata", metadata),
    Check("files.list_mkdir_remove", list_mkdir_remove),
    Check("files.file_transfer", file_transfer),
    Check("files.dir_transfer", dir_transfer),
]
