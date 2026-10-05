import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

import pr_body


def git(cwd: Path, *args: str) -> str:
    env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"}
    return subprocess.run(["git", *args], cwd=cwd, env=env, check=True, capture_output=True, text=True).stdout.strip()


class BuildTest(unittest.TestCase):
    def setUp(self) -> None:
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo = Path(tmp.name)
        git(self.repo, "init", "-q")
        cwd = os.getcwd()
        os.chdir(self.repo)
        self.addCleanup(os.chdir, cwd)

    def write(self, ts: str, py_record: str, py: str, changelogs: dict[str, str] | None = None) -> str:
        files = {
            "sdks/typescript/package.json": json.dumps({"name": "useshards", "version": ts}),
            "sdks/python/package.json": json.dumps({"name": "useshards-python", "version": py_record}),
            "sdks/python/src/useshards/_version.py": f'__version__ = "{py}"\n',
            **(changelogs or {}),
        }
        for path, text in files.items():
            (self.repo / path).parent.mkdir(parents=True, exist_ok=True)
            (self.repo / path).write_text(text)
        git(self.repo, "add", "-A")
        git(self.repo, "commit", "-q", "-m", "c")
        return git(self.repo, "rev-parse", "HEAD")

    def test_names_each_released_sdk_with_its_notes_and_registry(self) -> None:
        base = self.write("0.1.0-alpha.1", "0.1.0-alpha.1", "0.1.0a1")
        git(self.repo, "tag", "sdk-typescript-v0.1.0")
        head = self.write(
            "0.1.0",
            "0.2.0-alpha.0",
            "0.2.0a0",
            {
                "sdks/typescript/CHANGELOG.md": "# useshards\n\n## 0.1.0\n\n### Patch Changes\n\n- abc1234: First stable release.\n",
                "sdks/python/CHANGELOG.md": "# useshards-python\n\n## 0.2.0-alpha.0\n\n### Minor Changes\n\n- def5678: Streams.\n\n## 0.1.0\n\n- old\n",
            },
        )
        body = pr_body.build(base, head)
        self.assertIn("## useshards (TypeScript) 0.1.0-alpha.1 -> 0.1.0", body)
        self.assertIn("`useshards@0.1.0` to npm under the dist-tag `latest`. The tag `sdk-typescript-v0.1.0` exists already", body)
        self.assertIn("- abc1234: First stable release.", body)
        self.assertIn("## useshards (Python) 0.1.0a1 -> 0.2.0a0", body)
        self.assertIn("`useshards==0.2.0a0` to PyPI.", body)
        self.assertIn("- def5678: Streams.", body)
        self.assertNotIn("- old", body)

    def test_one_sdk_only(self) -> None:
        base = self.write("0.1.0", "0.1.0", "0.1.0")
        head = self.write("0.2.0-alpha.0", "0.1.0", "0.1.0")
        body = pr_body.build(base, head)
        self.assertIn("`useshards@0.2.0-alpha.0` to npm under the dist-tag `alpha`.", body)
        self.assertNotIn("(Python)", body)

    def test_no_version_change(self) -> None:
        base = self.write("0.1.0", "0.1.0", "0.1.0")
        self.assertIn("releases no SDK", pr_body.build(base, base))


if __name__ == "__main__":
    unittest.main()
