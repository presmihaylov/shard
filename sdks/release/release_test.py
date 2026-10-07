import io
import json
import subprocess
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path

import release
from release import ReleaseError, Sdk

NOTE = "### Patch Changes\n\n- abc1234: A note.\n"

UV_LOCK = """version = 1

[[package]]
name = "httpx"
version = "0.28.1"

[[package]]
name = "useshards"
version = "{version}"
source = {{ editable = "." }}
"""


def lock(version: str) -> str:
    body = {
        "name": "useshards",
        "version": version,
        "lockfileVersion": 3,
        "requires": True,
        "packages": {"": {"name": "useshards", "version": version}, "node_modules/x": {"version": "1.0.0"}},
    }
    return json.dumps(body, indent=2) + "\n"


def manifest(name: str, version: str, **extra: str) -> str:
    return json.dumps({"name": name, "version": version, **extra}, indent=2) + "\n"


class Repo:
    """A git repo with the two SDKs at 0.1.0-alpha.1, one commit on main."""

    def __init__(self, root: Path, config: bool = True) -> None:
        self.root = root
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.email", "release@test.invalid")
        self.git("config", "user.name", "release test")
        self.write("sdks/typescript/package.json", manifest("useshards", "0.1.0-alpha.1"))
        self.write("sdks/typescript/package-lock.json", lock("0.1.0-alpha.1"))
        self.write("sdks/typescript/src/version.ts", 'export const version = "0.1.0-alpha.1";\n')
        self.write("sdks/typescript/src/index.ts", "export {};\n")
        self.write("sdks/typescript/README.md", "# useshards\n")
        self.write("sdks/python/package.json", manifest("useshards-python", "0.1.0-alpha.1"))
        self.write("sdks/python/pyproject.toml", '[project]\nname = "useshards"\n')
        self.write("sdks/python/src/useshards/_version.py", '__version__ = "0.1.0a1"\n')
        self.write("sdks/python/src/useshards/__init__.py", "")
        self.write("sdks/python/uv.lock", UV_LOCK.format(version="0.1.0a1"))
        if config:
            self.write("sdks/.changeset/config.json", "{}\n")
            self.write("sdks/.changeset/README.md", "# Changesets\n")
        self.commit("base")

    def git(self, *args: str) -> str:
        return subprocess.run(["git", *args], cwd=self.root, check=True, capture_output=True, text=True).stdout

    def write(self, path: str, text: str) -> None:
        (self.root / path).parent.mkdir(parents=True, exist_ok=True)
        (self.root / path).write_text(text)

    def commit(self, message: str) -> str:
        self.git("add", "-A")
        self.git("commit", "-q", "--allow-empty", "-m", message)
        return self.git("rev-parse", "HEAD").strip()

    def branch(self, name: str) -> None:
        self.git("checkout", "-q", "-b", name)

    def changeset(self, name: str, *packages: str) -> None:
        front = "".join(f'"{package}": patch\n' for package in packages)
        self.write(f"sdks/.changeset/{name}.md", f"---\n{front}---\n\nA note.\n")

    def bump(self, ts: str | None, py: str | None) -> None:
        """What `npm run release-version` leaves: new manifests, the bridge's files, and a changelog per bumped SDK."""
        if ts is not None:
            self.write("sdks/typescript/package.json", manifest("useshards", ts))
            self.write("sdks/typescript/CHANGELOG.md", f"# useshards\n\n## {ts}\n\n{NOTE}")
        if py is not None:
            self.write("sdks/python/package.json", manifest("useshards-python", py))
            self.write("sdks/python/CHANGELOG.md", f"# useshards-python\n\n## {py}\n\n{NOTE}")
        release.sync(self.root)


class Lookups:
    def __init__(self, releases: dict[str, str] | None = None, registry: set[tuple[str, str]] | None = None) -> None:
        self.releases = releases or {}
        self.registry = registry or set()
        self.asked: list[str] = []

    def release(self, tag: str) -> str | None:
        self.asked.append(tag)
        return self.releases.get(tag)

    def has(self, sdk: Sdk, version: str) -> bool:
        return (sdk.key, version) in self.registry


class Pep440Test(unittest.TestCase):
    def test_maps_stable_and_each_prerelease(self) -> None:
        cases = {"0.1.0": "0.1.0", "0.1.0-alpha.1": "0.1.0a1", "1.2.3-beta.4": "1.2.3b4", "2.0.0-rc.0": "2.0.0rc0"}
        for semver, want in cases.items():
            self.assertEqual(release.pep440(semver), want, semver)

    def test_refuses_what_pep_440_cannot_say(self) -> None:
        for semver in ("0.1.0-next.1", "0.1", "0.1.0-alpha", "v0.1.0"):
            with self.assertRaises(ReleaseError, msg=semver):
                release.pep440(semver)


class TempRepoTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)
        self.repo = Repo(self.tmp)

    def read(self, path: str) -> str:
        return (self.tmp / path).read_text()


class SyncTest(TempRepoTest):
    def test_writes_every_version_changesets_does_not(self) -> None:
        self.repo.write("sdks/typescript/package.json", manifest("useshards", "0.1.0"))
        self.repo.write("sdks/python/package.json", manifest("useshards-python", "0.2.0-rc.3"))
        release.sync(self.tmp)

        self.assertEqual(self.read("sdks/typescript/package-lock.json"), lock("0.1.0"))
        self.assertEqual(self.read("sdks/typescript/src/version.ts"), 'export const version = "0.1.0";\n')
        self.assertEqual(self.read("sdks/python/src/useshards/_version.py"), '__version__ = "0.2.0rc3"\n')
        self.assertEqual(self.read("sdks/python/uv.lock"), UV_LOCK.format(version="0.2.0rc3"))

    def test_a_second_run_changes_nothing(self) -> None:
        self.repo.bump("0.1.0", "0.1.0")
        self.repo.commit("bumped")
        release.sync(self.tmp)
        self.assertEqual(self.repo.git("status", "--porcelain"), "")

    def test_refuses_a_lock_with_no_useshards_entry(self) -> None:
        self.repo.write("sdks/python/uv.lock", "version = 1\n")
        with self.assertRaisesRegex(ReleaseError, "uv.lock holds 0 version lines"):
            release.sync(self.tmp)

    def test_keeps_the_real_lock_byte_for_byte(self) -> None:
        real = (Path(__file__).resolve().parents[1] / "typescript" / "package-lock.json").read_text()
        self.assertEqual(json.dumps(json.loads(real), indent=2, ensure_ascii=False) + "\n", real)


class CheckTest(TempRepoTest):
    def setUp(self) -> None:
        super().setUp()
        self.repo.branch("feature")

    def check(self, branch: str = "feature") -> list[str]:
        self.repo.commit("change")
        return release.check(self.tmp, "main", branch)

    def test_a_base_without_changesets_is_exempt(self) -> None:
        bare = self.tmp / "bare"
        bare.mkdir()
        repo = Repo(bare, config=False)
        repo.branch("feature")
        repo.write("sdks/typescript/src/index.ts", "export const x = 1;\n")
        repo.write("sdks/typescript/package.json", manifest("useshards", "9.9.9"))
        repo.commit("change")
        self.assertEqual(release.check(bare, "main", "feature"), [])

    def test_a_source_change_needs_a_changeset(self) -> None:
        self.repo.write("sdks/typescript/src/index.ts", "export const x = 1;\n")
        problems = self.check()
        self.assertEqual(len(problems), 1)
        self.assertIn("need a changeset naming useshards", problems[0])

    def test_a_changeset_naming_the_sdk_passes(self) -> None:
        self.repo.write("sdks/python/src/useshards/__init__.py", "x = 1\n")
        self.repo.changeset("fix", "useshards-python")
        self.assertEqual(self.check(), [])

    def test_a_minor_or_major_bump_fails(self) -> None:
        self.repo.write("sdks/typescript/src/index.ts", "export const x = 1;\n")
        self.repo.write("sdks/.changeset/feature.md", '---\n"useshards": minor\n"useshards-python": major\n---\n\nA note.\n')
        problems = self.check()
        self.assertEqual(len(problems), 2, problems)
        self.assertIn("bumps useshards minor", problems[0])
        self.assertIn("bumps useshards-python major", problems[1])

    def test_a_changeset_for_the_other_sdk_does_not_cover_it(self) -> None:
        self.repo.write("sdks/python/pyproject.toml", '[project]\nname = "useshards"\ndependencies = []\n')
        self.repo.changeset("fix", "useshards")
        problems = self.check()
        self.assertEqual(len(problems), 1)
        self.assertIn("need a changeset naming useshards-python", problems[0])

    def test_an_empty_changeset_covers_both(self) -> None:
        self.repo.write("sdks/typescript/src/index.ts", "export const x = 1;\n")
        self.repo.write("sdks/python/src/useshards/__init__.py", "x = 1\n")
        # The exact file `npx changeset add --empty` writes: a blank line between the fences.
        self.repo.write("sdks/.changeset/internal.md", "---\n\n---\n\n\n")
        self.assertEqual(self.check(), [])

    def test_a_change_outside_the_shipped_files_needs_nothing(self) -> None:
        self.repo.write("sdks/typescript/README.md", "# useshards, now with words\n")
        self.assertEqual(self.check(), [])

    def test_a_dependency_change_in_package_json_needs_a_changeset(self) -> None:
        self.repo.write("sdks/typescript/package.json", manifest("useshards", "0.1.0-alpha.1", license="MIT"))
        self.assertEqual(len(self.check()), 1)

    def test_a_version_edit_off_the_release_branch_fails_once_per_file(self) -> None:
        self.repo.changeset("bump", "useshards", "useshards-python")
        self.repo.bump("0.1.0", "0.1.0")
        problems = self.check()
        self.assertEqual(len(problems), 6, problems)
        self.assertTrue(all("only the Release SDKs PR" in problem for problem in problems))

    def test_one_version_file_alone_counts_too(self) -> None:
        self.repo.write("sdks/python/uv.lock", UV_LOCK.format(version="0.1.0"))
        problems = self.check()
        self.assertEqual(len(problems), 1)
        self.assertIn("uv.lock changes the version from 0.1.0a1 to 0.1.0", problems[0])

    def test_the_release_branch_bumps_and_spends_its_changesets(self) -> None:
        self.repo.git("checkout", "-q", "main")
        self.repo.changeset("first", "useshards", "useshards-python")
        self.repo.commit("a changeset lands")
        self.repo.git("checkout", "-q", "-b", release.RELEASE_BRANCH)
        (self.tmp / "sdks/.changeset/first.md").unlink()
        self.repo.bump("0.1.0", "0.1.0")
        self.assertEqual(self.check(release.RELEASE_BRANCH), [])

    def test_a_fork_never_counts_as_the_release_branch(self) -> None:
        self.repo.bump("0.1.0", None)
        self.assertNotEqual(self.check("fork"), [])

    def test_a_malformed_changeset_fails_loudly(self) -> None:
        self.repo.write("sdks/.changeset/bad.md", "no front matter\n")
        self.repo.commit("bad")
        with self.assertRaisesRegex(ReleaseError, "front matter"):
            release.check(self.tmp, "main", "feature")


class PlanTest(TempRepoTest):
    def plan(self, lookups: Lookups | None = None) -> dict[str, str]:
        found = lookups or Lookups()
        return release.plan(self.tmp, found.release, found.has)

    def release_commit(self, ts: str | None, py: str | None) -> str:
        self.repo.bump(ts, py)
        return self.repo.commit("Release SDKs")

    def test_no_changelog_publishes_nothing(self) -> None:
        self.assertEqual(self.plan(), {"typescript": "false", "python": "false"})

    def test_typescript_only(self) -> None:
        sha = self.release_commit("0.1.0", None)
        self.assertEqual(
            self.plan(),
            {
                "typescript": "true",
                "typescript_version": "0.1.0",
                "typescript_tag": "sdk-typescript-v0.1.0",
                "typescript_ref": sha,
                "typescript_prerelease": "false",
                "typescript_npm_tag": "latest",
                "python": "false",
            },
        )

    def test_python_only(self) -> None:
        sha = self.release_commit(None, "0.1.0")
        self.assertEqual(
            self.plan(),
            {
                "typescript": "false",
                "python": "true",
                "python_version": "0.1.0",
                "python_tag": "sdk-python-v0.1.0",
                "python_ref": sha,
                "python_prerelease": "false",
            },
        )

    def test_both(self) -> None:
        sha = self.release_commit("0.1.0", "0.1.0")
        out = self.plan()
        self.assertEqual((out["typescript"], out["python"]), ("true", "true"))
        self.assertEqual((out["typescript_ref"], out["python_ref"]), (sha, sha))

    def test_none_once_both_are_out(self) -> None:
        self.release_commit("0.1.0", "0.1.0")
        lookups = Lookups(
            releases={"sdk-typescript-v0.1.0": "published", "sdk-python-v0.1.0": "published"},
            registry={("typescript", "0.1.0"), ("python", "0.1.0")},
        )
        self.assertEqual(self.plan(lookups), {"typescript": "false", "python": "false"})

    def test_reuses_a_published_release_the_registry_lacks(self) -> None:
        self.release_commit("0.1.0", "0.1.0")
        lookups = Lookups(releases={"sdk-typescript-v0.1.0": "published", "sdk-python-v0.1.0": "published"})
        out = self.plan(lookups)
        self.assertEqual((out["typescript_tag"], out["python_tag"]), ("sdk-typescript-v0.1.0", "sdk-python-v0.1.0"))
        self.assertEqual(lookups.asked, ["sdk-typescript-v0.1.0", "sdk-python-v0.1.0"])

    def test_retries_what_a_failed_run_left(self) -> None:
        sha = self.release_commit("0.1.0", "0.1.0")
        self.repo.write("docs.md", "later\n")
        self.repo.commit("a later commit on main")
        lookups = Lookups(releases={"sdk-typescript-v0.1.0": "published"}, registry={("typescript", "0.1.0")})
        out = self.plan(lookups)
        self.assertEqual((out["typescript"], out["python"], out["python_ref"]), ("false", "true", sha))

    def test_a_registry_copy_with_no_release_still_publishes(self) -> None:
        self.release_commit("0.1.0", None)
        self.assertEqual(self.plan(Lookups(registry={("typescript", "0.1.0")}))["typescript"], "true")

    def test_a_prerelease_goes_out_under_its_own_tag(self) -> None:
        self.release_commit("0.2.0-alpha.0", "0.2.0-alpha.0")
        out = self.plan()
        self.assertEqual(
            (out["typescript_npm_tag"], out["typescript_prerelease"], out["python_version"], out["python_prerelease"]),
            ("alpha", "true", "0.2.0a0", "true"),
        )
        self.assertEqual(out["python_tag"], "sdk-python-v0.2.0a0")

    def test_a_draft_release_fails(self) -> None:
        self.release_commit("0.1.0", None)
        with self.assertRaisesRegex(ReleaseError, "is a draft"):
            self.plan(Lookups(releases={"sdk-typescript-v0.1.0": "draft"}))

    def test_a_changelog_that_disagrees_with_the_version_fails(self) -> None:
        self.release_commit("0.1.0", None)
        self.repo.write("sdks/typescript/package.json", manifest("useshards", "0.1.1"))
        with self.assertRaisesRegex(ReleaseError, "opens with 0.1.0"):
            self.plan()

    def test_a_stale_version_py_fails(self) -> None:
        self.release_commit(None, "0.1.0")
        self.repo.write("sdks/python/src/useshards/_version.py", '__version__ = "0.1.0a1"\n')
        with self.assertRaisesRegex(ReleaseError, "_version.py says 0.1.0a1"):
            self.plan()

    def test_the_ref_is_the_first_parent_commit_that_set_the_version(self) -> None:
        first = self.release_commit("0.1.0", "0.1.0")
        self.repo.write("sdks/typescript/package.json", manifest("useshards", "0.1.0", license="MIT"))
        self.repo.commit("no version change")
        self.assertEqual(self.plan()["typescript_ref"], first)


def tgz(files: dict[str, bytes], mtime: int = 0) -> bytes:
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode="w:gz") as archive:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size, info.mtime = len(data), mtime
            archive.addfile(info, io.BytesIO(data))
    return out.getvalue()


def whl(files: dict[str, bytes], when: tuple[int, int, int, int, int, int] = (1980, 1, 1, 0, 0, 0)) -> bytes:
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w") as archive:
        for name, data in files.items():
            archive.writestr(zipfile.ZipInfo(name, when), data)
    return out.getvalue()


class CompareTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.tmp = Path(tmp.name)

    def pair(self, suffix: str, a: bytes, b: bytes) -> tuple[Path, Path]:
        left, right = self.tmp / f"a{suffix}", self.tmp / f"b{suffix}"
        left.write_bytes(a)
        right.write_bytes(b)
        return left, right

    def test_same_members_in_a_different_archive_are_equal(self) -> None:
        files = {"package/package.json": b"{}", "package/dist/index.js": b"x"}
        self.assertEqual(release.compare(*self.pair(".tgz", tgz(files, 1), tgz(files, 499162500))), [])
        self.assertEqual(release.compare(*self.pair(".whl", whl(files), whl(files, (2026, 10, 5, 0, 0, 0)))), [])

    def test_names_each_difference(self) -> None:
        a = {"package/package.json": b"{}", "package/dist/index.js": b"x", "package/old.js": b""}
        b = {"package/package.json": b"{}", "package/dist/index.js": b"y", "package/new.js": b""}
        self.assertEqual(
            release.compare(*self.pair(".tar.gz", tgz(a), tgz(b))),
            ["only in a.tar.gz: package/old.js", "only in b.tar.gz: package/new.js", "differs: package/dist/index.js"],
        )

    def test_refuses_an_unknown_archive(self) -> None:
        with self.assertRaisesRegex(ReleaseError, "is not a"):
            release.compare(*self.pair(".rar", b"", b""))


if __name__ == "__main__":
    unittest.main()
