from __future__ import annotations

import io
import itertools
import os
import stat
import tarfile
from pathlib import Path

import pytest

from useshards import _archive
from useshards.errors import ProtocolError, UnsafeArchiveError

Entry = tuple[tarfile.TarInfo, bytes]


def entry(name: str, kind: bytes = tarfile.REGTYPE, *, data: bytes = b"", mode: int = 0o644, link: str = "") -> Entry:
    info = tarfile.TarInfo(name)
    info.type = kind
    info.mode = mode
    info.linkname = link
    info.size = len(data) if kind == tarfile.REGTYPE else 0
    return info, data


def tar(*entries: Entry) -> io.BytesIO:
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode="w", format=tarfile.PAX_FORMAT) as t:
        for info, data in entries:
            t.addfile(info, io.BytesIO(data) if info.isreg() else None)
    out.seek(0)
    return out


def top(*rest: Entry) -> io.BytesIO:
    return tar(entry("top", tarfile.DIRTYPE, mode=0o750), *rest)


def unpacked(tmp_path: Path, source: io.BytesIO) -> Path:
    dst = tmp_path / "dst"
    dst.mkdir()
    _archive.unpack(source, str(dst), "top")
    return dst


def refused(tmp_path: Path, source: io.BytesIO, reason: str) -> Path:
    dst = tmp_path / "dst"
    dst.mkdir()
    with pytest.raises(UnsafeArchiveError, match=reason):
        _archive.unpack(source, str(dst), "top")
    return dst


def leftovers(dst: Path) -> list[str]:
    return [p.name for p in dst.rglob(".useshards-unpack-*")]


def test_round_trip(tmp_path: Path) -> None:
    source = tmp_path / "source"
    (source / "nested" / "deeper").mkdir(parents=True)
    (source / "top.txt").write_bytes(b"top\n")
    (source / "nested" / "deeper" / "leaf.sh").write_bytes(b"#!/bin/sh\n")
    (source / "nested" / "deeper" / "leaf.sh").chmod(0o750)
    (source / "nested" / "empty").write_bytes(b"")
    (source / "nested" / "link").symlink_to("deeper/leaf.sh")
    (source / "nested").chmod(0o711)
    packed = io.BytesIO()
    _archive.pack(str(source), "top", packed)
    packed.seek(0)
    dst = unpacked(tmp_path, packed)
    assert (dst / "top.txt").read_bytes() == b"top\n"
    assert (dst / "nested" / "empty").read_bytes() == b""
    assert stat.S_IMODE((dst / "nested" / "deeper" / "leaf.sh").stat().st_mode) == 0o750
    assert stat.S_IMODE((dst / "nested").stat().st_mode) == 0o711
    assert os.readlink(dst / "nested" / "link") == "deeper/leaf.sh"
    assert (dst / "nested" / "link").read_bytes() == b"#!/bin/sh\n"
    assert leftovers(dst) == []


def test_setid_dropped_sticky_kept(tmp_path: Path) -> None:
    dst = unpacked(
        tmp_path,
        top(entry("top/run", data=b"x", mode=0o6755), entry("top/shared", tarfile.DIRTYPE, mode=0o1777)),
    )
    assert stat.S_IMODE((dst / "run").stat().st_mode) == 0o755
    assert stat.S_IMODE((dst / "shared").stat().st_mode) == 0o1777


@pytest.mark.parametrize(
    ("name", "reason"),
    [
        ("top/../evil", "a .. component"),
        ("/top/evil", "an absolute name"),
        ("other/evil", "it is not under top"),
    ],
)
def test_names_refused(tmp_path: Path, name: str, reason: str) -> None:
    refused(tmp_path, top(entry(name, data=b"x")), reason)
    assert not (tmp_path / "evil").exists()


@pytest.mark.parametrize("target", ["/etc/passwd", "../outside", "a/../../outside"])
def test_symlink_that_leaves(tmp_path: Path, target: str) -> None:
    dst = refused(tmp_path, top(entry("top/link", tarfile.SYMTYPE, link=target)), "which leaves the destination")
    assert not (dst / "link").is_symlink()


def test_symlink_chain_confined(tmp_path: Path) -> None:
    (tmp_path / "outside").write_bytes(b"secret")
    source = top(
        entry("top/here", tarfile.SYMTYPE, link="."),
        entry("top/escape", tarfile.SYMTYPE, link="here/../outside"),
    )
    dst = refused(tmp_path, source, "through another link")
    assert not (dst / "escape").is_symlink()
    assert (dst / "here").is_symlink()


def test_never_writes_through_a_symlink(tmp_path: Path) -> None:
    source = top(
        entry("top/sub", tarfile.DIRTYPE),
        entry("top/link", tarfile.SYMTYPE, link="sub"),
        entry("top/link/file", data=b"x"),
    )
    dst = refused(tmp_path, source, "under a symlink")
    assert not (dst / "sub" / "file").exists()


def test_hard_link_to_a_file_it_wrote(tmp_path: Path) -> None:
    dst = unpacked(tmp_path, top(entry("top/a", data=b"one"), entry("top/b", tarfile.LNKTYPE, link="top/a")))
    assert os.path.samefile(dst / "a", dst / "b")


@pytest.mark.parametrize("link", ["top/missing", "/etc/passwd", "top/../x"])
def test_hard_link_refused(tmp_path: Path, link: str) -> None:
    refused(tmp_path, top(entry("top/b", tarfile.LNKTYPE, link=link)), "not a file earlier in the archive")


@pytest.mark.parametrize(
    ("kind", "reason"),
    [(tarfile.CHRTYPE, "a device node"), (tarfile.BLKTYPE, "a device node"), (tarfile.FIFOTYPE, "a fifo")],
)
def test_special_files_refused(tmp_path: Path, kind: bytes, reason: str) -> None:
    refused(tmp_path, top(entry("top/special", kind)), reason)


def test_only_a_directory_lands_at_the_destination(tmp_path: Path) -> None:
    refused(tmp_path, tar(entry("top", data=b"x")), "only a directory")


def test_directory_over_a_file_refused(tmp_path: Path) -> None:
    refused(tmp_path, top(entry("top/x", data=b"x"), entry("top/x", tarfile.DIRTYPE)), "something else already is")


def test_byte_cap(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(_archive, "MAX_BYTES", 4)
    dst = refused(tmp_path, top(entry("top/a", data=b"abc"), entry("top/b", data=b"de")), "runs past 4 bytes")
    assert leftovers(dst) == []


def negative(size: int) -> io.BytesIO:
    """top/a with a GNU base-256 size of size, then top/b, which must never land."""
    out = top(entry("top/a"), entry("top/b", data=b"hello"))
    raw = bytearray(out.getvalue())
    header = next(i for i in range(0, len(raw), 512) if raw[i : i + 6] == b"top/a\0")
    raw[header + 124 : header + 136] = b"\xff" + (256**11 + size).to_bytes(11, "big")
    raw[header + 148 : header + 156] = b" " * 8
    raw[header + 148 : header + 156] = b"%06o\0 " % sum(raw[header : header + 512])
    return io.BytesIO(bytes(raw))


def test_negative_size_refused(tmp_path: Path) -> None:
    with tarfile.open(fileobj=negative(-1)) as t:
        assert t.getmember("top/a").size == -1
    dst = refused(tmp_path, negative(-1), "a negative size")
    assert not (dst / "b").exists()
    assert leftovers(dst) == []


def test_occupied_temp_name_kept(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    dst = tmp_path / "dst"
    dst.mkdir()
    theirs = dst / ".useshards-unpack-taken"
    theirs.write_bytes(b"theirs")
    taken = itertools.cycle([True, False])
    fresh = itertools.count()
    monkeypatch.setattr(_archive, "token_hex", lambda _: "taken" if next(taken) else f"fresh{next(fresh)}")
    source = top(
        entry("top/a", data=b"hello"),
        entry("top/s", tarfile.SYMTYPE, link="a"),
        entry("top/h", tarfile.LNKTYPE, link="top/a"),
    )
    _archive.unpack(source, str(dst), "top")
    assert theirs.read_bytes() == b"theirs"
    assert (dst / "a").read_bytes() == b"hello"
    assert (dst / "s").read_bytes() == b"hello"
    assert (dst / "h").read_bytes() == b"hello"
    assert leftovers(dst) == [theirs.name]


def test_entry_cap(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(_archive, "MAX_ENTRIES", 2)
    refused(tmp_path, top(entry("top/a"), entry("top/b")), "runs past 2 entries")


def test_unreadable_tar(tmp_path: Path) -> None:
    dst = tmp_path / "dst"
    dst.mkdir()
    with pytest.raises(ProtocolError, match="cannot read"):
        _archive.unpack(io.BytesIO(b"not a tar" * 100), str(dst), "top")
