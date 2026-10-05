"""Write the body of the Release SDKs pull request: each SDK it releases, the old and new version, its notes and its registry."""

import argparse
import json
import re
import subprocess
import sys
from dataclasses import dataclass


@dataclass(frozen=True)
class SDK:
    key: str
    title: str
    record: str
    shipped: str
    changelog: str
    registry: str


SDKS = (
    SDK(
        key="typescript",
        title="useshards (TypeScript)",
        record="sdks/typescript/package.json",
        shipped="sdks/typescript/package.json",
        changelog="sdks/typescript/CHANGELOG.md",
        registry="npm",
    ),
    SDK(
        key="python",
        title="useshards (Python)",
        record="sdks/python/package.json",
        shipped="sdks/python/src/useshards/_version.py",
        changelog="sdks/python/CHANGELOG.md",
        registry="PyPI",
    ),
)

VERSION_PY = re.compile(r'^__version__ = "([^"]+)"$', re.MULTILINE)


def show(rev: str, path: str) -> str | None:
    out = subprocess.run(["git", "show", f"{rev}:{path}"], capture_output=True, text=True)
    if out.returncode != 0:
        return None
    return out.stdout


def tag_exists(tag: str) -> bool:
    out = subprocess.run(["git", "rev-parse", "-q", "--verify", f"refs/tags/{tag}"], capture_output=True, text=True)
    return out.returncode == 0


def shipped_version(sdk: SDK, text: str | None) -> str | None:
    if text is None:
        return None
    if sdk.shipped.endswith(".json"):
        return json.loads(text)["version"]
    match = VERSION_PY.search(text)
    if match is None:
        raise SystemExit(f"{sdk.shipped} holds no __version__ line")
    return match.group(1)


def notes(changelog: str | None, version: str) -> str:
    if changelog is None:
        return ""
    lines = changelog.splitlines()
    try:
        start = lines.index(f"## {version}") + 1
    except ValueError:
        return ""
    end = next((i for i in range(start, len(lines)) if lines[i].startswith("## ")), len(lines))
    return "\n".join(lines[start:end]).strip()


def publishes(sdk: SDK, version: str) -> str:
    if sdk.registry == "npm":
        dist_tag = "alpha" if "-" in version else "latest"
        return f"Publishes `useshards@{version}` to npm under the dist-tag `{dist_tag}`."
    return f"Publishes `useshards=={version}` to PyPI."


def section(sdk: SDK, old: str | None, new: str, record: str, changelog: str | None) -> str:
    tag = f"sdk-{sdk.key}-v{new}"
    lines = [f"## {sdk.title} {old or 'unreleased'} -> {new}", "", publishes(sdk, new)]
    if tag_exists(tag):
        lines[-1] += f" The tag `{tag}` exists already, so the run reuses it and makes no new one."
    body = notes(changelog, record)
    if body:
        lines += ["", body]
    return "\n".join(lines)


def build(base: str, head: str) -> str:
    sections = []
    for sdk in SDKS:
        old = shipped_version(sdk, show(base, sdk.shipped))
        new = shipped_version(sdk, show(head, sdk.shipped))
        if new is None or new == old:
            continue
        record = json.loads(show(head, sdk.record) or "{}").get("version", new)
        sections.append(section(sdk, old, new, record, show(head, sdk.changelog)))

    if not sections:
        return "This PR releases no SDK: its changesets bump no version.\n"

    head_lines = [
        "Merging this PR publishes the SDKs below. Nothing publishes before the merge, and SDK changes that land on main meanwhile update this PR.",
    ]
    tail_lines = [
        "Each SDK gets its tag and GitHub release only if it lacks them, and none is marked latest: that stays with the shard binary release.",
        "CI and SDK release run on this branch by dispatch, since a push by GITHUB_TOKEN starts no workflow.",
    ]
    return "\n\n".join([*head_lines, *sections, *tail_lines]) + "\n"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True, help="the revision the release branch starts from")
    parser.add_argument("--head", required=True, help="the release branch revision")
    args = parser.parse_args()
    sys.stdout.write(build(args.base, args.head))


if __name__ == "__main__":
    main()
