"""The SDK release tooling: the version bridge, the changeset check, the publish plan and the artifact compare."""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import tarfile
import urllib.error
import urllib.request
import zipfile
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
RELEASE_BRANCH = "changeset-release/main"
CHANGESET_CONFIG = "sdks/.changeset/config.json"


class ReleaseError(Exception):
    pass


@dataclass(frozen=True)
class Sdk:
    key: str
    package: str
    manifest: str
    changelog: str
    source: tuple[str, ...]


TYPESCRIPT = Sdk(
    key="typescript",
    package="useshards",
    manifest="sdks/typescript/package.json",
    changelog="sdks/typescript/CHANGELOG.md",
    source=("sdks/typescript/src/", "sdks/typescript/package.json"),
)
PYTHON = Sdk(
    key="python",
    package="useshards-python",
    manifest="sdks/python/package.json",
    changelog="sdks/python/CHANGELOG.md",
    source=("sdks/python/src/", "sdks/python/pyproject.toml"),
)
SDKS = (TYPESCRIPT, PYTHON)

TS_LOCK = "sdks/typescript/package-lock.json"
TS_CONST = "sdks/typescript/src/version.ts"
PY_CONST = "sdks/python/src/useshards/_version.py"
UV_LOCK = "sdks/python/uv.lock"

SEMVER = re.compile(r"^(\d+)\.(\d+)\.(\d+)(?:-(alpha|beta|rc)\.(\d+))?$")
PEP440_PRE = {"alpha": "a", "beta": "b", "rc": "rc"}
TS_CONST_RE = re.compile(r'(export const version = ")([^"]*)(";)')
PY_CONST_RE = re.compile(r'(__version__ = ")([^"]*)(")')
UV_LOCK_RE = re.compile(r'(\[\[package\]\]\nname = "useshards"\nversion = ")([^"]*)(")')


def pep440(semver: str) -> str:
    m = SEMVER.match(semver)
    if m is None:
        raise ReleaseError(f"{semver} is not X.Y.Z or X.Y.Z-(alpha|beta|rc).N, so it has no PEP 440 form")
    major, minor, patch, pre, n = m.groups()
    if pre is None:
        return f"{major}.{minor}.{patch}"
    return f"{major}.{minor}.{patch}{PEP440_PRE[pre]}{n}"


def prerelease_id(semver: str) -> str | None:
    m = SEMVER.match(semver)
    if m is None:
        raise ReleaseError(f"{semver} is not X.Y.Z or X.Y.Z-(alpha|beta|rc).N")
    return m.group(4)


def manifest_version(text: str) -> str:
    version = json.loads(text).get("version")
    if not isinstance(version, str):
        raise ReleaseError("the manifest has no string version")
    return version


def one_match(pattern: re.Pattern[str], text: str, path: str) -> re.Match[str]:
    found = list(pattern.finditer(text))
    if len(found) != 1:
        raise ReleaseError(f"{path} holds {len(found)} version lines, not 1")
    return found[0]


def lock_version(text: str) -> str:
    lock = json.loads(text)
    return f'{lock.get("version")} {lock.get("packages", {}).get("", {}).get("version")}'


# Every file that carries an SDK version, and how to read it; only the Release SDKs branch changes one.
VERSION_READERS: dict[str, Callable[[str], str]] = {
    TYPESCRIPT.manifest: manifest_version,
    TS_LOCK: lock_version,
    TS_CONST: lambda text: one_match(TS_CONST_RE, text, TS_CONST).group(2),
    PYTHON.manifest: manifest_version,
    PY_CONST: lambda text: one_match(PY_CONST_RE, text, PY_CONST).group(2),
    UV_LOCK: lambda text: one_match(UV_LOCK_RE, text, UV_LOCK).group(2),
}


def without_version(path: str, text: str) -> str:
    if path == TYPESCRIPT.manifest:
        manifest = json.loads(text)
        manifest.pop("version", None)
        return json.dumps(manifest, sort_keys=True)
    if path == TS_CONST:
        return TS_CONST_RE.sub(r"\1\3", text)
    if path == PY_CONST:
        return PY_CONST_RE.sub(r"\1\3", text)
    return text


def git(root: Path, *args: str) -> str:
    done = subprocess.run(["git", *args], cwd=root, capture_output=True, text=True)
    if done.returncode != 0:
        raise ReleaseError(f"git {' '.join(args)}: {done.stderr.strip()}")
    return done.stdout


def show(root: Path, ref: str, path: str) -> str | None:
    if not git(root, "ls-tree", "--name-only", ref, "--", path).strip():
        return None
    return git(root, "show", f"{ref}:{path}")


def read(root: Path, path: str) -> str:
    return (root / path).read_text(encoding="utf-8")


def write(root: Path, path: str, text: str) -> None:
    (root / path).write_text(text, encoding="utf-8")


def sync(root: Path) -> list[str]:
    ts = manifest_version(read(root, TYPESCRIPT.manifest))
    py = pep440(manifest_version(read(root, PYTHON.manifest)))

    lock = json.loads(read(root, TS_LOCK))
    lock["version"] = ts
    lock["packages"][""]["version"] = ts
    write(root, TS_LOCK, json.dumps(lock, indent=2, ensure_ascii=False) + "\n")

    for path, pattern, version in ((TS_CONST, TS_CONST_RE, ts), (PY_CONST, PY_CONST_RE, py), (UV_LOCK, UV_LOCK_RE, py)):
        text = read(root, path)
        one_match(pattern, text, path)
        write(root, path, pattern.sub(rf"\g<1>{version}\g<3>", text))

    return [f"typescript {ts}: {TS_LOCK}, {TS_CONST}", f"python {py}: {PY_CONST}, {UV_LOCK}"]


def changeset_packages(text: str, path: str) -> set[str]:
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        raise ReleaseError(f"{path} does not open with a --- front matter line")
    names: set[str] = set()
    for line in lines[1:]:
        if line.strip() == "---":
            return names
        if not line.strip():
            continue
        name, sep, _ = line.partition(":")
        if not sep:
            raise ReleaseError(f"{path}: {line!r} is not 'package: bump'")
        names.add(name.strip().strip("\"'"))
    raise ReleaseError(f"{path} never closes its front matter")


def check(root: Path, base: str, branch: str) -> list[str]:
    if show(root, base, CHANGESET_CONFIG) is None:
        return []

    merge_base = git(root, "merge-base", base, "HEAD").strip()
    changed = git(root, "diff", "--name-only", merge_base, "HEAD").split()
    problems: list[str] = []

    for path, reader in VERSION_READERS.items():
        old, new = show(root, merge_base, path), show(root, "HEAD", path)
        was = reader(old) if old is not None else None
        now = reader(new) if new is not None else None
        if was != now and branch != RELEASE_BRANCH:
            problems.append(
                f"{path} changes the version from {was} to {now}; only the Release SDKs PR ({RELEASE_BRANCH}) does that"
            )

    named: set[str] = set()
    empty = False
    for path in changed:
        if not path.startswith("sdks/.changeset/") or not path.endswith(".md") or path.endswith("/README.md"):
            continue
        text = show(root, "HEAD", path)
        if text is None:
            continue
        packages = changeset_packages(text, path)
        named |= packages
        empty = empty or not packages

    for sdk in SDKS:
        touched = [
            path for path in changed if path.startswith(sdk.source) and not same_but_version(root, merge_base, path)
        ]
        if touched and sdk.package not in named and not empty:
            problems.append(
                f"{sdk.key} changes ({', '.join(touched[:3])}) need a changeset naming {sdk.package}: "
                "run `npx changeset` in sdks/, or `npx changeset --empty` for a change no user sees"
            )

    return problems


def same_but_version(root: Path, ref: str, path: str) -> bool:
    if path not in (TYPESCRIPT.manifest, TS_CONST, PY_CONST):
        return False
    old, new = show(root, ref, path), show(root, "HEAD", path)
    if old is None or new is None:
        return False
    return without_version(path, old) == without_version(path, new)


def release_record(root: Path, sdk: Sdk) -> str | None:
    path = root / sdk.changelog
    if not path.exists():
        return None
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.startswith("## "):
            return line[3:].strip()
    return None


def version_ref(root: Path, path: str, reader: Callable[[str], str]) -> str:
    for commit in git(root, "log", "--first-parent", "--format=%H", "HEAD", "--", path).split():
        parents = git(root, "rev-list", "--parents", "-n", "1", commit).split()[1:]
        old = show(root, parents[0], path) if parents else None
        new = show(root, commit, path)
        was = reader(old) if old is not None else None
        now = reader(new) if new is not None else None
        if was != now:
            return commit
    raise ReleaseError(f"no first-parent commit of HEAD sets the version in {path}")


# A release lookup answers None, "draft" or "published"; a registry lookup answers whether it holds the version.
Releases = Callable[[str], str | None]
Registry = Callable[[Sdk, str], bool]


def plan(root: Path, releases: Releases, registry: Registry) -> dict[str, str]:
    out: dict[str, str] = {}
    for sdk in SDKS:
        semver = manifest_version(read(root, sdk.manifest))
        record = release_record(root, sdk)
        out[sdk.key] = "false"
        if record is None:
            continue
        if record != semver:
            raise ReleaseError(f"{sdk.changelog} opens with {record}, but {sdk.manifest} says {semver}")

        version, ref_path = semver, sdk.manifest
        reader: Callable[[str], str] = manifest_version
        if sdk is PYTHON:
            version = pep440(semver)
            ref_path, reader = PY_CONST, VERSION_READERS[PY_CONST]
            const = reader(read(root, PY_CONST))
            if const != version:
                raise ReleaseError(f"{PY_CONST} says {const}, but {sdk.manifest} {semver} reads {version} in PEP 440")

        tag = f"sdk-{sdk.key}-v{version}"
        release = releases(tag)
        if release == "draft":
            raise ReleaseError(f"the release {tag} is a draft: publish or delete it by hand, then run this again")
        if release == "published" and registry(sdk, version):
            continue

        pre = prerelease_id(semver)
        out.update(
            {
                sdk.key: "true",
                f"{sdk.key}_version": version,
                f"{sdk.key}_tag": tag,
                f"{sdk.key}_ref": version_ref(root, ref_path, reader),
                f"{sdk.key}_prerelease": "true" if pre else "false",
            }
        )
        if sdk is TYPESCRIPT:
            out["typescript_npm_tag"] = pre or "latest"
    return out


def gh_releases(repo: str) -> Releases:
    def lookup(tag: str) -> str | None:
        query = f'.[] | select(.tag_name == "{tag}") | .draft'
        args = ["gh", "api", "--paginate", f"repos/{repo}/releases", "--jq", query]
        done = subprocess.run(args, capture_output=True, text=True)
        if done.returncode != 0:
            raise ReleaseError(f"list the releases of {repo}: {done.stderr.strip()}")
        drafts = done.stdout.split()
        if not drafts:
            return None
        return "published" if "false" in drafts else "draft"

    return lookup


def registry_has(sdk: Sdk, version: str) -> bool:
    url = f"https://registry.npmjs.org/useshards/{version}"
    if sdk is PYTHON:
        url = f"https://pypi.org/pypi/useshards/{version}/json"
    try:
        with urllib.request.urlopen(url, timeout=30) as response:
            status: int = response.status
            return status == 200
    except urllib.error.HTTPError as err:
        if err.code == 404:
            return False
        raise ReleaseError(f"ask {url}: {err}") from err
    except urllib.error.URLError as err:
        raise ReleaseError(f"ask {url}: {err}") from err


def members(path: Path) -> dict[str, bytes]:
    name = path.name
    if name.endswith((".whl", ".zip")):
        with zipfile.ZipFile(path) as archive:
            return {info.filename: archive.read(info) for info in archive.infolist() if not info.is_dir()}
    if name.endswith((".tgz", ".tar.gz")):
        found: dict[str, bytes] = {}
        with tarfile.open(path, "r:gz") as archive:
            for info in archive.getmembers():
                if not info.isfile():
                    continue
                stream = archive.extractfile(info)
                if stream is None:
                    raise ReleaseError(f"{path}: cannot read {info.name}")
                found[info.name] = stream.read()
        return found
    raise ReleaseError(f"{path} is not a .tgz, .tar.gz, .whl or .zip")


def compare(a: Path, b: Path) -> list[str]:
    left, right = members(a), members(b)
    problems = [f"only in {a.name}: {name}" for name in sorted(left.keys() - right.keys())]
    problems += [f"only in {b.name}: {name}" for name in sorted(right.keys() - left.keys())]
    problems += [f"differs: {name}" for name in sorted(left.keys() & right.keys()) if left[name] != right[name]]
    return problems


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    verbs = parser.add_subparsers(dest="verb", required=True)
    verbs.add_parser("sync", help="write each version Changesets does not: the TS lock and constant, Python's two")
    checker = verbs.add_parser("check", help="an SDK change has a changeset; only the release branch edits a version")
    checker.add_argument("--base", required=True)
    checker.add_argument("--branch", required=True)
    verbs.add_parser("plan", help="which SDK to publish, written to GITHUB_OUTPUT")
    comparer = verbs.add_parser("compare", help="the two archives hold the same members with the same bytes")
    comparer.add_argument("a", type=Path)
    comparer.add_argument("b", type=Path)
    args = parser.parse_args(argv)

    try:
        return run(args)
    except ReleaseError as err:
        print(f"error: {err}", file=sys.stderr)
        return 1


def run(args: argparse.Namespace) -> int:
    if args.verb == "sync":
        for line in sync(ROOT):
            print(line)
        return 0

    if args.verb == "check":
        problems = check(ROOT, args.base, args.branch)
        for problem in problems:
            print(f"error: {problem}", file=sys.stderr)
        return 1 if problems else 0

    if args.verb == "compare":
        problems = compare(args.a, args.b)
        for problem in problems:
            print(problem, file=sys.stderr)
        return 1 if problems else 0

    repo = os.environ.get("GITHUB_REPOSITORY")
    if not repo:
        raise ReleaseError("GITHUB_REPOSITORY is not set")
    out = plan(ROOT, gh_releases(repo), registry_has)
    lines = "".join(f"{key}={value}\n" for key, value in out.items())
    print(lines, end="")
    target = os.environ.get("GITHUB_OUTPUT")
    if target:
        with open(target, "a", encoding="utf-8") as f:
            f.write(lines)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
