"""Pack a local directory as a tar, and unpack a sandbox's tar under a directory it never writes outside of."""

from __future__ import annotations

import errno
import os
import posixpath
import shutil
import stat
import tarfile
from collections.abc import Callable
from secrets import token_hex
from typing import IO, TypeVar

from .errors import ProtocolError, UnsafeArchiveError

# The Go client's caps, since a sandbox's tar is untrusted.
MAX_BYTES = 64 << 30
MAX_ENTRIES = 1 << 20
_CHUNK = 1 << 20
_CREATE = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_BINARY", 0)

T = TypeVar("T")


def pack(source: str, name: str, out: IO[bytes]) -> None:
    """Write source as a tar whose top entry is name, a directory's entries in lexical order."""
    with tarfile.open(fileobj=out, mode="w|", format=tarfile.PAX_FORMAT) as tar:
        # The walk starts past a link the source names, as an open of a file follows one.
        tar.add(os.path.realpath(source), arcname=name)


def unpack(source: IO[bytes], dst: str, strip: str) -> None:
    """Land the tar under dst, the entry named strip at dst itself, by the rules of the Go client's unpack."""
    _Unpacker(dst, strip).run(source)


class _Unpacker:
    def __init__(self, dst: str, strip: str) -> None:
        self.dst = os.path.abspath(dst)
        self.real = os.path.realpath(dst)
        self.strip = strip
        self.bytes = 0
        # The regular files this unpack wrote, by identity, the only targets a hard link may name.
        self.files: dict[str, os.stat_result] = {}
        self.dirs: list[tuple[str, int]] = []
        self.links: list[tuple[str, str]] = []

    def run(self, source: IO[bytes]) -> None:
        try:
            self._entries(source)
        except BaseException as e:
            # A refused unpack leaves no link behind that leaves dst either.
            for err in self._confine():
                e.add_note(str(err))
            raise
        refused = self._confine()
        if refused:
            for err in refused[1:]:
                refused[0].add_note(str(err))
            raise refused[0]
        # Deepest first, so each directory's entries landed whatever its own mode is.
        for at, mode in reversed(self.dirs):
            os.chmod(at, mode)

    def _entries(self, source: IO[bytes]) -> None:
        try:
            with tarfile.open(fileobj=source, mode="r|") as tar:
                for count, member in enumerate(tar):
                    if count >= MAX_ENTRIES:
                        raise UnsafeArchiveError(member.name, f"the archive runs past {MAX_ENTRIES} entries")
                    self._entry(tar, member)
        except tarfile.TarError as e:
            raise ProtocolError(f"the sandbox sent a tar the SDK cannot read: {e}") from e

    def _entry(self, tar: tarfile.TarFile, member: tarfile.TarInfo) -> None:
        name = self._local(member.name)
        if name == "." and not member.isdir():
            raise UnsafeArchiveError(member.name, "only a directory can land at the destination itself")
        if member.isdir():
            return self._dir(name, member)
        if member.isreg():
            return self._file(name, member, tar)
        if member.issym():
            return self._symlink(name, member)
        if member.islnk():
            return self._hardlink(name, member)
        if member.ischr() or member.isblk():
            raise UnsafeArchiveError(member.name, "a device node")
        if member.isfifo():
            raise UnsafeArchiveError(member.name, "a fifo, which an unpack does not make")
        raise UnsafeArchiveError(member.name, f"the type {member.type!r}, which an unpack does not make")

    def _local(self, name: str) -> str:
        """The entry's name under dst: no absolute name and no .. before the clean, then strip cut off."""
        if not name:
            raise UnsafeArchiveError(name, "an empty name")
        if name.startswith("/"):
            raise UnsafeArchiveError(name, "an absolute name")
        if ".." in name.split("/"):
            raise UnsafeArchiveError(name, "a .. component")
        clean = posixpath.normpath(name)
        if clean == self.strip:
            return "."
        prefix = self.strip + "/"
        if not clean.startswith(prefix):
            raise UnsafeArchiveError(name, f"it is not under {self.strip}")
        return clean[len(prefix) :]

    def _parent(self, name: str, entry: str) -> str:
        """Make name's directories as mkdir -p does, through no symlink, and answer the path name lands at."""
        if name == ".":
            return self.dst
        at = self.dst
        parts = name.split("/")
        for part in parts[:-1]:
            at = os.path.join(at, part)
            try:
                info = os.lstat(at)
            except FileNotFoundError:
                os.mkdir(at, 0o755)
                continue
            if stat.S_ISLNK(info.st_mode):
                raise UnsafeArchiveError(entry, "it sits under a symlink, which an unpack never writes through")
            if not stat.S_ISDIR(info.st_mode):
                raise NotADirectoryError(errno.ENOTDIR, os.strerror(errno.ENOTDIR), at)
        target = os.path.join(at, parts[-1])
        # A name the platform reads otherwise, as a backslash on Windows, must still land under dst.
        if os.path.commonpath([self.dst, os.path.abspath(target)]) != self.dst:
            raise UnsafeArchiveError(entry, "it leaves the destination")
        return target

    def _dir(self, name: str, member: tarfile.TarInfo) -> None:
        at = self._parent(name, member.name)
        try:
            # Only its owner can write it until the tree is in, so its entries land whatever its mode.
            os.mkdir(at, 0o700)
        except FileExistsError:
            if not stat.S_ISDIR(os.lstat(at).st_mode):
                raise UnsafeArchiveError(member.name, "a directory where something else already is") from None
            return
        self.dirs.append((at, _mode(member)))

    def _file(self, name: str, member: tarfile.TarInfo, tar: tarfile.TarFile) -> None:
        # A GNU base-256 size can be negative, which would wind the byte count back under the cap.
        if member.size < 0:
            raise UnsafeArchiveError(member.name, "a negative size")
        if member.size > MAX_BYTES - self.bytes:
            raise UnsafeArchiveError(member.name, f"the archive runs past {MAX_BYTES} bytes")
        self.bytes += member.size
        at = self._parent(name, member.name)
        data = tar.extractfile(member)
        if data is None:
            raise ProtocolError(f"the tar holds no data for the file {member.name!r}")
        tmp, fd = _fresh(at, lambda p: os.open(p, _CREATE, 0o600))
        try:
            with os.fdopen(fd, "wb") as out:
                shutil.copyfileobj(data, out, _CHUNK)
            os.chmod(tmp, _mode(member))
        except BaseException:
            _remove_temp(tmp)
            raise
        self._place(tmp, at, name, file=True)

    def _symlink(self, name: str, member: tarfile.TarInfo) -> None:
        if _leaves(name, member.linkname):
            raise UnsafeArchiveError(member.name, f"a symlink to {member.linkname!r}, which leaves the destination")
        at = self._parent(name, member.name)
        tmp = _fresh(at, lambda p: os.symlink(member.linkname, p))[0]
        self.links.append((at, member.name))
        self._place(tmp, at, name, file=False)

    def _hardlink(self, name: str, member: tarfile.TarInfo) -> None:
        refusal = UnsafeArchiveError(
            member.name, f"a hard link to {member.linkname!r}, which is not a file earlier in the archive"
        )
        try:
            target = self._local(member.linkname)
        except UnsafeArchiveError:
            raise refusal from None
        wrote = self.files.get(target)
        if wrote is None:
            raise refusal
        at = self._parent(name, member.name)
        tmp = _fresh(at, lambda p: os.link(os.path.join(self.dst, *target.split("/")), p))[0]
        # A filesystem that folds case lets a later entry spelled another way take the name.
        linked = os.lstat(tmp)
        if not stat.S_ISREG(linked.st_mode) or not os.path.samestat(wrote, linked):
            _remove_temp(tmp)
            raise UnsafeArchiveError(
                member.name, f"a hard link to {member.linkname!r}, which no longer names the file the archive wrote"
            )
        self._place(tmp, at, name, file=True)

    def _place(self, tmp: str, at: str, name: str, *, file: bool) -> None:
        """Rename the finished temp over the name, so a link already there is replaced and never followed."""
        try:
            os.replace(tmp, at)
        except BaseException:
            _remove_temp(tmp)
            raise
        if not file:
            self.files.pop(name, None)
            return
        self.files[name] = os.lstat(at)

    def _confine(self) -> list[Exception]:
        """Remove every new link that leaves dst through another one, which only the tree as written shows."""
        refused: list[Exception] = []
        for at, entry in self.links:
            if os.path.commonpath([self.real, os.path.realpath(at)]) == self.real:
                continue
            refused.append(UnsafeArchiveError(entry, "a symlink that leaves the destination through another link"))
            try:
                os.remove(at)
            except OSError as e:
                refused.append(e)
        self.links = []
        return refused


def _leaves(name: str, target: str) -> bool:
    """The lexical half of the link check: an absolute target, or a .. that climbs past dst from where the link sits."""
    if posixpath.isabs(target):
        return True
    resolved = posixpath.normpath(posixpath.join(posixpath.dirname(name), target))
    return resolved == ".." or resolved.startswith("../")


def _mode(member: tarfile.TarInfo) -> int:
    # The host drops setuid and setgid, so a sandbox cannot hand it a set-id binary.
    return member.mode & (0o777 | stat.S_ISVTX)


def _fresh(at: str, make: Callable[[str], T]) -> tuple[str, T]:
    """A temp next to at that make creates and that was not there before, so a cleanup only removes its own."""
    while True:
        tmp = os.path.join(os.path.dirname(at), f".useshards-unpack-{token_hex(8)}")
        try:
            return tmp, make(tmp)
        except FileExistsError:
            continue


def _remove_temp(tmp: str) -> None:
    try:
        os.remove(tmp)
    except FileNotFoundError:
        return
