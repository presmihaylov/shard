"""Cross-SDK checks prove each release claim against one daemon."""

from __future__ import annotations

import argparse
import functools
import hashlib
import io
import json
import os
import re
import secrets
import shutil
import socket
import ssl
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable, Iterator
from dataclasses import dataclass
from pathlib import Path
from typing import Any

GATE = Path(__file__).resolve().parent
MiB = 1 << 20

# Every line is unique and ten bytes long, so the newest 8 MiB of the 20 MiB has one right answer.
CAPTURE_LINES = 2 * MiB
CAPTURE_LIMIT = 8 * MiB
CAPTURE_WRITER = f"awk 'BEGIN{{for(i=0;i<{CAPTURE_LINES};i++) printf \"%09d\\n\", i}}'"

# A key in a public answer that names how the host runs a sandbox, or how the daemon is administered.
DENIED_KEY = re.compile(
    r"(^|_)(pids?|cgroups?|netns|veth|bridge|sock|socket|root|rootfs|root_?dir|state_?(dir|path|file)|data_?dir"
    r"|bundles?|checkpoints?|registr(y|ies)|signing|ledger|tokens?|config_?(path|file)|build_?host|hostname"
    r"|daemon|runtime_?(path|root))($|_)",
    re.IGNORECASE,
)
HOST_PATHS = (
    "/var/lib/shard",
    "/run/shard",
    "/run/netns",
    "/run/runc",
    "/run/runsc",
    "/run/sysbox",
    "/sys/fs/cgroup",
    "/proc/",
    "/opt/shard",
    "/etc/shard",
    "/usr/local/bin/shard",
    "/usr/local/sbin/",
    "shard.sock",
    "daemon.lock",
    "signing-key",
    "serve.tokens",
)
# A host field may appear only in an object every key of which one of these schemas declares.
PUBLIC_USES = {"address": ("EgressDecision", "Port", "HostAddress"), "path": ("MkdirRequest",)}
NDJSON = "application/x-ndjson"
# A provider may refuse these verbs, so their suite lines may skip; every other check must pass.
MAY_SKIP = {"lifecycle.pause_resume", "lifecycle.fork", "lifecycle.unsupported_named"}
# The routes of those verbs, each by the capability that says whether this provider serves it.
MAY_REFUSE = {
    "POST /v0/sandboxes/{id}/pause": "pause",
    "POST /v0/sandboxes/{id}/resume": "resume",
    "POST /v0/sandboxes/{id}/fork": "fork",
}
SUITES = ("typescript", "python_sync", "python_async")
CAPTURE_STEP = {
    "typescript": ("ts", "capture"),
    "python_sync": ("py", "capture"),
    "python_async": ("py", "capture-async"),
}

# Each release claim, by name, and the lines that prove it: a gate check, or suite:<check> in all three runs.
CLAIMS: dict[str, list[str]] = {
    "packages build and install": ["install.typescript", "install.python"],
    "examples run": ["examples.typescript", "examples.python_sync", "examples.python_async"],
    "lifecycle": ["suite:lifecycle.*"],
    "snapshots": ["suite:snapshots.*"],
    "files": ["suite:files.*", "cross.files_ts_to_py", "cross.files_py_to_ts"],
    "authentication": ["suite:auth.*"],
    "command streams": ["suite:commands.*", "suite:logs.*", "suite:apps.*"],
    "reconnect": ["suite:commands.reconnect", "cross.reconnect_ts_to_py", "cross.reconnect_py_to_ts"],
    "cancellation": ["suite:commands.cancel_keeps_remote", "cross.cancel_ts_seen_by_py", "cross.cancel_py_seen_by_ts"],
    "output capture": ["suite:capture.*", *(f"capture.last_8mib_{run}" for run in SUITES)],
    "nonzero exits return results": ["suite:commands.nonzero_result", "cross.nonzero_result"],
    "a client cancel leaves the remote command running": ["cross.cancel_ts_seen_by_py", "cross.cancel_py_seen_by_ts"],
    "the capture keeps the newest 8 MiB of 20 MiB": [f"capture.last_8mib_{run}" for run in SUITES],
    "a * token reaches no local route": ["suite:auth.local_routes_refused", "leak.local_routes"],
    "the spec and responses hold no local detail": ["leak.spec", "leak.responses"],
    "secrets and policies": ["suite:secrets.*", "suite:policies.*"],
    "port forwards": ["suite:ports.*"],
}


class Failed(Exception):
    """A check that found its claim false."""


@dataclass
class Verdict:
    verdict: str
    detail: str = ""


@dataclass
class Answer:
    status: int
    headers: dict[str, str]
    body: bytes

    def json(self) -> Any:
        return json.loads(self.body)


def expect(ok: bool, message: str) -> None:
    if not ok:
        raise Failed(message)


def tail(text: str, lines: int = 8) -> str:
    return " | ".join(text.strip().splitlines()[-lines:])


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args: Any, **kwargs: Any) -> None:
        return None


class Gate:
    def __init__(self, tree: Path, out: Path, only: set[str]) -> None:
        self.tree = tree
        self.out = out
        self.only = only
        self.remote = os.environ["SHARD_REMOTE"].rstrip("/")
        self.key = os.environ["SHARD_API_KEY"]
        self.wildcard = os.environ["SHARD_SUITE_WILDCARD_KEY"]
        self.image = os.environ.get("SHARD_SUITE_IMAGE") or "alpine:3.20"
        self.host_paths = (*HOST_PATHS, *filter(None, os.environ.get("SHARD_GATE_HOST_PATHS", "").split(":")))
        self.work = Path(tempfile.mkdtemp(prefix="shard-gate-"))
        self.tag = secrets.token_hex(3)
        self.results: dict[str, Verdict] = {}
        self.suite_lines: dict[str, dict[str, Verdict]] = {}
        self.listed = [line for line in (tree / "sdks" / "suite" / "checks.txt").read_text().splitlines() if line]
        self.sandboxes: list[str] = []
        self.owned: list[str] = []
        self.shared: str | None = None
        self.leaks: list[str] = []
        self.host_fields = set(host_fields())
        self.secret_value = f"gate-secret-{secrets.token_hex(8)}"
        self.swept: list[tuple[str, str, int]] = []
        self.capabilities: dict[str, Any] = {}
        self.opener = urllib.request.build_opener(NoRedirect(), urllib.request.HTTPSHandler(context=self.tls()))
        self.answers = 0

    def tls(self) -> ssl.SSLContext:
        ca = os.environ.get("SHARD_CA_FILE")
        return ssl.create_default_context(cafile=ca) if ca else ssl.create_default_context()

    def redact(self, text: str) -> str:
        for value, name in ((self.key, "<SHARD_API_KEY>"), (self.wildcard, "<SHARD_SUITE_WILDCARD_KEY>")):
            text = text.replace(value, name)
        return text

    def say(self, line: str) -> None:
        print(self.redact(line), flush=True)

    def check(self, name: str, run: Callable[[], str | None]) -> None:
        # Every other check needs the packages, so --only never leaves the installs out.
        if self.only and name not in self.only and not name.startswith("install."):
            return
        started = time.monotonic()
        try:
            detail = run() or ""
            self.results[name] = Verdict("PASS", detail)
        except Failed as e:
            self.results[name] = Verdict("FAIL", str(e))
        except Exception as e:
            self.results[name] = Verdict("FAIL", f"{type(e).__name__}: {e}")
        verdict = self.results[name]
        took = f" ({time.monotonic() - started:.0f}s)"
        self.say(f"{verdict.verdict} {name}{': ' + verdict.detail if verdict.detail else ''}{took}")

    def needs(self, *names: str) -> None:
        for name in names:
            got = self.results.get(name)
            expect(got is not None and got.verdict == "PASS", f"needs {name}, which did not pass")

    def run(self, argv: list[str], cwd: Path, log: str, env: dict[str, str] | None = None, timeout: int = 900) -> str:
        """Run argv, keep its output under the out dir as log, and return its stdout."""
        proc = self.spawn(argv, cwd, log, env, timeout)
        if proc.returncode != 0:
            raise Failed(
                f"{Path(argv[0]).name} exited {proc.returncode}: {self.redact(tail(proc.stderr or proc.stdout))}"
            )
        return proc.stdout

    def spawn(
        self, argv: list[str], cwd: Path, log: str, env: dict[str, str] | None, timeout: int
    ) -> subprocess.CompletedProcess[str]:
        try:
            proc = subprocess.run(
                argv,
                cwd=cwd,
                env={**os.environ, **(env or {})},
                capture_output=True,
                text=True,
                timeout=timeout,
                check=False,
            )
        except FileNotFoundError as e:
            raise Failed(f"{argv[0]} is not on PATH") from e
        except subprocess.TimeoutExpired as e:
            raise Failed(f"{' '.join(argv[:3])} ran past {timeout}s") from e
        record = f"$ {' '.join(argv)}\n--- stdout\n{proc.stdout}\n--- stderr\n{proc.stderr}\n"
        with (self.out / f"{log}.log").open("a") as f:
            f.write(self.redact(record))
        return proc

    # Install: each package as a user gets it, in a project of its own, and its examples run against that install.

    def install_typescript(self) -> str:
        sdk = self.tree / "sdks" / "typescript"
        expect(sdk.is_dir(), f"{sdk} does not exist")
        self.run(["npm", "ci", "--no-audit", "--no-fund"], sdk, "install-typescript")
        self.run(["npm", "pack", "--pack-destination", str(self.work)], sdk, "install-typescript")
        tarballs = sorted(self.work.glob("useshards-*.tgz"))
        expect(len(tarballs) == 1, f"npm pack made {len(tarballs)} tarballs")
        tarball = tarballs[0]
        project = self.work / "ts"
        project.mkdir()
        (project / "package.json").write_text(json.dumps({"name": "gate", "private": True, "type": "module"}))
        self.run(["npm", "install", "--no-audit", "--no-fund", str(tarball)], project, "install-typescript")
        shutil.copy(GATE / "actor.mjs", project / "actor.mjs")
        self.run(
            ["node", "-e", "import('useshards').then((m) => { if (!m.Shard) process.exit(1) })"],
            project,
            "install-typescript",
        )
        return tarball.name

    def install_python(self) -> str:
        sdk = self.tree / "sdks" / "python"
        expect(sdk.is_dir(), f"{sdk} does not exist")
        dist = self.work / "dist"
        self.run(["uv", "build", "--wheel", "--out-dir", str(dist)], sdk, "install-python")
        wheels = sorted(dist.glob("useshards-*.whl"))
        expect(len(wheels) == 1, f"uv build made {len(wheels)} wheels")
        venv = self.work / "venv"
        self.run(["uv", "venv", "--python", ">=3.11", str(venv)], self.work, "install-python")
        self.run(["uv", "pip", "install", "--python", str(self.python), str(wheels[0])], self.work, "install-python")
        where = self.run(
            [str(self.python), "-c", "import useshards; print(useshards.__file__)"], self.work, "install-python"
        )
        expect(str(venv) in where, f"useshards imports from {where.strip()}, not the venv")
        return wheels[0].name

    @property
    def python(self) -> Path:
        return self.work / "venv" / "bin" / "python"

    @functools.cached_property
    def spec(self) -> Any:
        return json.loads((self.tree / "docs" / "openapi.json").read_text())

    def example_typescript(self) -> None:
        self.needs("install.typescript")
        project = self.work / "ts"
        shutil.copy(self.tree / "sdks" / "typescript" / "examples" / "quickstart.ts", project / "quickstart.ts")
        tools = self.tree / "sdks" / "typescript" / "node_modules"
        # The example typechecks against the installed declarations, as a user's editor would read them.
        self.run(
            [
                str(tools / ".bin" / "tsc"),
                "--noEmit",
                "--strict",
                "--target",
                "es2022",
                "--module",
                "nodenext",
                "--moduleResolution",
                "nodenext",
                "--types",
                "node",
                "--typeRoots",
                str(tools / "@types"),
                "quickstart.ts",
            ],
            project,
            "examples",
        )
        self.run([str(tools / ".bin" / "tsx"), "quickstart.ts"], project, "examples")

    def example_python(self, script: str) -> None:
        self.needs("install.python")
        copied = self.work / script
        shutil.copy(self.tree / "sdks" / "python" / "examples" / script, copied)
        self.run([str(self.python), str(copied)], self.work, "examples")

    # Suites: the shared checks of sdks/suite/checks.txt, each line kept so a claim can name it.

    def suite(self, run: str) -> str:
        if run == "typescript":
            return self.suite_in(run, ["npm", "run", "suite"], self.tree / "sdks" / "typescript", {})
        mode = run.removeprefix("python_")
        argv = ["uv", "run", "--locked", "python", "-m", "suite"]
        return self.suite_in(run, argv, self.tree / "sdks" / "python", {"SHARD_SUITE_MODE": mode})

    def suite_in(self, run: str, argv: list[str], cwd: Path, env: dict[str, str]) -> str:
        expect(cwd.is_dir(), f"{cwd} does not exist")
        lines: dict[str, Verdict] = {}
        self.suite_lines[run] = lines
        # An inherited SHARD_SUITE_ONLY would run part of the suite, and a claim needs every check of it.
        proc = self.spawn(argv, cwd, f"suite-{run}", {**env, "SHARD_SUITE_ONLY": ""}, 3600)
        for line in proc.stdout.splitlines():
            verdict, _, rest = line.partition(" ")
            if verdict not in ("PASS", "FAIL", "SKIP"):
                continue
            name, _, detail = rest.partition(": ")
            lines[name] = Verdict(verdict, detail)
            self.say(f"  {run} {line}")
        failed = [name for name, line in lines.items() if line.verdict == "FAIL"]
        skipped = [name for name, line in lines.items() if line.verdict == "SKIP" and name not in MAY_SKIP]
        unrun = [name for name in self.listed if name not in lines]
        expect(not unrun, f"{len(unrun)} of {len(self.listed)} never ran: {', '.join(unrun)}")
        expect(not failed, f"{len(failed)} failed: {', '.join(failed)}")
        expect(not skipped, f"skipped where no provider may: {', '.join(skipped)}")
        expect(proc.returncode == 0, f"exited {proc.returncode}: {self.redact(tail(proc.stderr))}")
        return f"{len(lines)} lines"

    # Cross checks: one SDK acts, the other observes, each in a process of its own.

    def actor(self, sdk: str, *args: str) -> dict[str, Any]:
        if sdk == "ts":
            self.needs("install.typescript")
            return self.answer(self.run(["node", "actor.mjs", *args], self.work / "ts", "actors", timeout=600))
        self.needs("install.python")
        return self.answer(
            self.run([str(self.python), str(GATE / "actor.py"), *args], self.work, "actors", timeout=600)
        )

    def answer(self, stdout: str) -> dict[str, Any]:
        got: dict[str, Any] = json.loads(stdout.strip().splitlines()[-1])
        return got

    def sandbox(self) -> str:
        if self.shared is None:
            self.shared = str(self.actor("py", "create", self.image, f"gate-{self.tag}")["id"])
            self.sandboxes.append(self.shared)
        return self.shared

    def marker(self, what: str) -> str:
        return f"gate-{what}-{secrets.token_hex(4)}"

    def cancel(self, canceller: str, observer: str, error: str) -> str:
        sid, marker = self.sandbox(), self.marker("cancel")
        got = self.actor(canceller, "cancel", sid, marker)
        expect(got["cancelled"] == error, f"the {canceller} exec ended as {got['cancelled']}, not by the cancel")
        seen = self.actor(observer, "find", sid, marker)
        expect(seen["state_at_list"] == "running", f"the {observer} client listed it as {seen['state_at_list']}")
        expect(seen["exit_code"] == 0, f"it exited {seen['exit_code']}")
        expect(seen["stdout"] == "started\n", f"its stdout read {seen['stdout']!r}")
        expect(seen["file"] == f"{marker}\n", f"its marker file read {seen['file']!r}")
        return f"{canceller} cancelled, {observer} saw it run on and exit 0"

    def reconnect(self, starter: str, attacher: str) -> str:
        sid, marker = self.sandbox(), self.marker("reconnect")
        command = str(self.actor(starter, "start", sid, marker)["command"])
        got = self.actor(attacher, "attach", sid, command)
        expect(got["exit_code"] == 0, f"it exited {got['exit_code']}")
        expect(got["stdout"] == f"one\n{marker}\n", f"the {attacher} client read {got['stdout']!r}")
        return f"{attacher} read all of the {starter} command"

    def files(self, writer: str, reader: str) -> str:
        sid = self.sandbox()
        data = secrets.token_bytes(3 * MiB + 7)
        local = self.work / f"put-{writer}.bin"
        local.write_bytes(data)
        remote = f"/tmp/{self.marker('files')}.bin"
        self.actor(writer, "put", sid, str(local), remote)
        got = self.actor(reader, "get", sid, remote)
        expect(got["size"] == len(data), f"{reader} read {got['size']} bytes, want {len(data)}")
        expect(got["sha256"] == hashlib.sha256(data).hexdigest(), f"{reader} read other bytes than {writer} wrote")
        return f"{len(data)} bytes"

    def nonzero(self) -> str:
        sid = self.sandbox()
        script = "echo out; echo err >&2; exit 7"
        want = {"exit_code": 7, "stdout": "out\n", "stderr": "err\n"}
        for sdk in ("ts", "py"):
            got = self.actor(sdk, "exec", sid, script)
            expect(got == want, f"{sdk} returned {got}, want {want}")
        return "both return exit 7 with its output"

    def capture(self, run: str, want: tuple[int, str]) -> str:
        sid = self.sandbox()
        sdk, step = CAPTURE_STEP[run]
        got = self.actor(sdk, step, sid, CAPTURE_WRITER, str(CAPTURE_LIMIT))
        total, digest = want
        expect(got["exit_code"] == 0, f"the writer exited {got['exit_code']}")
        expect(got["seen"] == total, f"the callbacks saw {got['seen']} bytes, want {total}")
        expect(got["stdout_len"] == CAPTURE_LIMIT, f"stdout kept {got['stdout_len']} bytes, want {CAPTURE_LIMIT}")
        expect(got["stdout_sha256"] == digest, "stdout is not the newest 8 MiB")
        expect(got["stderr_len"] == 0, f"stderr kept {got['stderr_len']} bytes")
        expect(got["lost_bytes"] == 0, f"the daemon lost {got['lost_bytes']} bytes")
        return f"the last {CAPTURE_LIMIT} of {total} bytes, exactly"

    # Leaks: the spec, every public answer, and every local route through the front.

    def request(
        self,
        method: str,
        path: str,
        key: str | None = "",
        body: bytes | None = None,
        sent: dict[str, str] | None = None,
    ) -> Answer:
        headers = dict(sent or {})
        token = self.key if key == "" else key
        if token is not None:
            headers["Authorization"] = f"Bearer {token}"
        req = urllib.request.Request(self.remote + path, data=body, method=method, headers=headers)
        try:
            with self.opener.open(req, timeout=120) as resp:
                return Answer(resp.status, dict(resp.headers.items()), resp.read())
        except urllib.error.HTTPError as e:
            return Answer(e.code, dict(e.headers.items()), e.read())

    def record(self, method: str, path: str, answer: Answer) -> None:
        self.answers += 1
        name = re.sub(r"[^A-Za-z0-9.-]+", "_", f"{self.answers:03d}-{method}-{path}")[:120]
        shown = {"request": f"{method} {path}", "status": answer.status, "headers": answer.headers}
        text = answer.body.decode("utf-8", "replace")
        (self.out / "responses" / f"{name}.json").write_text(self.redact(json.dumps({**shown, "body": text}, indent=1)))

    def leaks_in(self, where: str, answer: Answer) -> list[str]:
        found = []
        raw = answer.body.decode("utf-8", "replace") + "\n" + "\n".join(answer.headers.values())
        values = (
            ("SHARD_API_KEY", self.key),
            ("SHARD_SUITE_WILDCARD_KEY", self.wildcard),
            ("secret", self.secret_value),
        )
        for secret_name, value in values:
            if value in raw:
                found.append(f"{where} holds the {secret_name} value")
        found += [f"{where} holds {path}" for path in self.host_paths if path in raw]
        kind = answer.headers.get("Content-Type", "")
        if "json" not in kind or not answer.body:
            return found
        lines = answer.body.splitlines() if NDJSON in kind else [answer.body]
        try:
            bodies = [json.loads(line) for line in lines if line.strip()]
        except json.JSONDecodeError:
            return [*found, f"{where} says it is JSON and is not"]
        for body in bodies:
            found += [f"{where} has the key {key}" for key in keys(body) if DENIED_KEY.search(key)]
            found += [f"{where} has the host field {key}" for key in self.host_fields_in(body)]
        return found

    def host_fields_in(self, body: Any) -> Iterator[str]:
        for each in objects(body):
            for key in sorted(self.host_fields & each.keys()):
                if not any(each.keys() <= self.declared(schema) for schema in PUBLIC_USES.get(key, ())):
                    yield key

    def declared(self, schema: str) -> set[str]:
        return set(self.spec["components"]["schemas"][schema].get("properties", {}))

    def leak_spec(self) -> str:
        spec_file = self.tree / "docs" / "openapi.json"
        text = spec_file.read_text()
        spec = json.loads(text)
        found = [f"the spec holds {path}" for path in self.host_paths if path in text]
        found += [f"the spec has the property {name}" for name in properties(spec) if DENIED_KEY.search(name)]
        # A renamed schema must fail here by name, not as a KeyError in leak.responses.
        found += [f"PUBLIC_USES names {schema}, which the spec does not declare" for uses in PUBLIC_USES.values() for schema in uses if schema not in spec["components"]["schemas"]]
        found += [
            f"the spec has the host field {name} in {owner or 'an inline schema'}"
            for owner, name in owned_properties(spec)
            if name in self.host_fields and owner not in PUBLIC_USES.get(name, ())
        ]
        for _, pattern in local_routes():
            prefix = pattern.split("{")[0].rstrip("/")
            if prefix in text:
                found.append(f"the spec mentions the local route {prefix}")
        expect(not found, "; ".join(sorted(set(found))))
        return f"{len(spec['paths'])} paths, {len(properties(spec))} property names"

    def leak_local_routes(self) -> str:
        refusal = self.request("GET", f"/v0/gate-no-such-route-{self.tag}")
        expect(refusal.status == 403, f"an unknown route answered {refusal.status}, want 403")
        tokens = (("SHARD_API_KEY", self.key), ("SHARD_SUITE_WILDCARD_KEY", self.wildcard))
        image = f"gate-no-such-image-{self.tag}:none"
        paths = [(method, pattern.replace("{ref...}", image)) for method, pattern in local_routes()]
        probes = [(method, variant, token) for method, path in paths for variant in variants(path) for token in tokens]
        found: list[str] = []
        for method, variant, (key_name, key) in probes:
            found += self.probe(method, variant, key_name, key, refusal)
        expect(not found, "; ".join(found))
        return f"{len(probes)} probes, each refused like an unknown route"

    def probe(self, method: str, path: str, key_name: str, key: str, refusal: Answer) -> list[str]:
        got = self.request(method, path, key)
        found = self.leaks_in(f"{method} {path}", got)
        if 300 <= got.status < 400:
            target = urllib.parse.urlsplit(got.headers.get("Location", ""))
            got = self.request(method, target.path + (f"?{target.query}" if target.query else ""), key)
        if got.status != 403 or got.body != refusal.body:
            found.append(f"{method} {path} with {key_name} answered {got.status} {got.body[:120]!r}")
        return found

    def sweep(
        self,
        method: str,
        path: str,
        body: Any = None,
        key: str | None = "",
        raw: bytes | None = None,
        sent: dict[str, str] | None = None,
    ) -> Answer:
        headers: dict[str, str] = dict(sent or {})
        data = raw
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        got = self.request(method, path, key, data, headers)
        self.record(method, path, got)
        self.swept.append((method, path.split("?")[0], got.status))
        self.leaks.extend(self.leaks_in(f"{method} {path} ({got.status})", got))
        return got

    def leak_responses(self) -> str:
        (self.out / "responses").mkdir(exist_ok=True)
        made = {"image": self.image, "name": f"gate-sweep-{self.tag}"}
        created = self.sweep("POST", "/v0/sandboxes?wait=true", made, sent={"Accept": NDJSON})
        last = json.loads(created.body.splitlines()[-1]) if created.status == 201 and created.body.strip() else {}
        expect("sandbox" in last, f"the streamed create answered {created.status} {created.body[-200:]!r}")
        sid = str(last["sandbox"]["id"])
        self.sandboxes.append(sid)
        box = f"/v0/sandboxes/{sid}"
        self.sweep_reads(box)
        self.sweep_execs(box, sid)
        self.sweep_files(box)
        self.sweep_grants(box, sid)
        self.sweep_ports(box)
        self.sweep_app()
        self.sweep_errors(box)
        if self.sweep("DELETE", f"{box}?force=true").status < 300:
            self.sandboxes.remove(sid)
        expect(not self.leaks, "; ".join(self.leaks))
        missing = list(self.unanswered())
        expect(not missing, f"{len(missing)} public routes never answered with success: {'; '.join(missing)}")
        return f"{self.answers} answers recorded under {self.out / 'responses'}"

    def unanswered(self) -> Iterator[str]:
        """Each public route of the spec no probe of the sweep got a success from, unless this provider refuses it."""
        for template, ops in self.spec["paths"].items():
            pattern = route_pattern(template)
            for method in (each.upper() for each in ops):
                got = [status for m, path, status in self.swept if m == method and pattern.fullmatch(path)]
                capability = MAY_REFUSE.get(f"{method} {template}")
                if got and capability and self.capabilities.get(capability) is False:
                    continue
                if not any(200 <= status < 300 or status == 101 for status in got):
                    yield f"{method} {template} ({', '.join(map(str, got)) or 'never probed'})"

    def sweep_reads(self, box: str) -> None:
        self.capabilities = self.sweep("GET", "/v0/capabilities").json()
        for path in ("/v0/version", "/v0/scopes", "/v0/sandboxes", box, f"{box}/logs"):
            self.sweep("GET", path)

    def sweep_execs(self, box: str, sid: str) -> None:
        # An exec runs only once a client attaches, so this one stays created and is never waited on.
        made = self.sweep("POST", f"{box}/exec", {"command": ["true"]})
        self.sweep("GET", f"{box}/exec")
        if made.status == 201:
            self.sweep("GET", f"{box}/exec/{made.json()['exec']}")
        held = f"{box}/exec/{self.actor('py', 'hold', sid)['command']}"
        self.sweep("POST", f"{held}/resize", {"rows": 40, "cols": 120})
        self.sweep("POST", f"{held}/kill", {"signal": "KILL"})
        self.sweep("GET", f"{held}?wait=true")
        self.sweep("DELETE", held)

    def sweep_files(self, box: str) -> None:
        file, archive = f"{box}/files?path=/tmp/gate-sweep.txt", f"{box}/archive?path=/tmp/gate-sweep-dir"
        self.sweep("PUT", file, raw=b"sweep\n", sent={"Content-Type": "application/octet-stream"})
        for method, path in (("GET", file), ("HEAD", file), ("GET", f"{box}/ls?path=/tmp")):
            self.sweep(method, path)
        self.sweep("POST", f"{box}/mkdir", {"path": "/tmp/gate-sweep-dir"})
        tar = tar_of("gate-sweep.txt", b"sweep\n")
        self.sweep("PUT", archive, raw=tar, sent={"Content-Type": "application/x-tar"})
        self.sweep("GET", archive)
        self.sweep("DELETE", file)

    def sweep_grants(self, box: str, sid: str) -> None:
        """A policy and a secret, each set and read, held by the stopped sandbox, then taken back and removed."""
        policy, secret = f"gate-policy-{self.tag}", f"GATE_SECRET_{self.tag.upper()}"
        self.owned += [f"/v0/secrets/{secret}?force=true", f"/v0/policies/{policy}"]
        self.sweep("PUT", f"/v0/policies/{policy}", {"rules": [{"action": "deny", "rule": "any"}]})
        self.sweep("PUT", f"/v0/secrets/{secret}", {"value": self.secret_value, "destinations": ["api.example.com"]})
        for path in (f"/v0/policies/{policy}", "/v0/policies", "/v0/secrets"):
            self.sweep("GET", path)
        self.sweep("POST", f"{box}/stop")
        self.sweep("PUT", f"{box}/policy", {"policy": policy})
        self.sweep("POST", f"{box}/secrets/{secret}")
        self.sweep("POST", f"{box}/start")
        self.sweep("GET", box)
        # Under deny-all the guest's connection leaves a record, so the egress log answers the one public address field.
        self.actor("py", "exec", sid, "wget -q -T 5 -O /dev/null http://192.0.2.1/ || true")
        self.sweep("GET", f"{box}/egress-log")
        self.sweep("POST", f"{box}/stop")
        self.sweep("DELETE", f"{box}/policy")
        self.sweep("DELETE", f"{box}/secrets/{secret}")
        self.sweep("DELETE", f"/v0/secrets/{secret}")
        self.sweep("DELETE", f"/v0/policies/{policy}")
        self.sweep("POST", f"{box}/start")

    def sweep_ports(self, box: str) -> None:
        """A private forward of the running sandbox, so its answer names the addresses the host listens on."""
        forward = f"{box}/ports/{free_port()}"
        self.sweep("PUT", forward, {"guest_port": 8000})
        for path in (f"{box}/ports", "/v0/ports"):
            self.sweep("GET", path)
        self.sweep("DELETE", forward)

    def sweep_app(self) -> None:
        made = {"image": self.image, "name": f"gate-app-{self.tag}", "command": ["sleep", "300"]}
        app = self.sweep("POST", "/v0/sandboxes?wait=true", made)
        expect(app.status == 201, f"the app's create answered {app.status}")
        aid = str(app.json()["id"])
        self.sandboxes.append(aid)
        ab = f"/v0/sandboxes/{aid}"
        self.sweep("GET", f"{ab}/logs")
        self.sweep("POST", f"{ab}/app/stop", {})
        # The app has ended, so a plain attach answers its exit at once.
        self.sweep("GET", f"{ab}/attach")
        self.sweep("POST", f"{ab}/stop")
        ref = f"gate-snap-{self.tag}"
        self.owned.append(f"/v0/snapshots/{ref}")
        self.sweep("POST", "/v0/snapshots", {"sandbox": aid, "name": ref})
        for path in ("/v0/snapshots", f"/v0/snapshots/{ref}"):
            self.sweep("GET", path)
        self.sweep("POST", f"{ab}/start")
        for verb in ("pause", "resume"):
            self.sweep("POST", f"{ab}/{verb}")
        fork = self.sweep("POST", f"{ab}/fork", {"name": f"gate-fork-{self.tag}"})
        if fork.status == 201:
            self.sandboxes.append(str(fork.json()["id"]))
        self.sweep("DELETE", f"/v0/snapshots/{ref}")
        if self.sweep("DELETE", f"{ab}?force=true").status < 300:
            self.sandboxes.remove(aid)

    def sweep_errors(self, box: str) -> None:
        """The refusals and errors a client meets, each of which could carry the host's detail in its message."""
        self.sweep("GET", f"/v0/sandboxes/gate-no-such-{self.tag}")
        self.sweep("GET", "/v0/sandboxes", key="gate-not-a-token")
        self.sweep("GET", "/v0/sandboxes", key=None)
        self.sweep("GET", f"/v0/gate-no-such-route-{self.tag}")
        self.sweep("POST", "/v0/sandboxes", raw=b"{", sent={"Content-Type": "application/json"})
        unpullable = self.sweep("POST", "/v0/sandboxes", {"image": f"gate.invalid/none-{self.tag}:none"})
        if unpullable.status < 300:
            self.sandboxes.append(str(unpullable.json()["id"]))
        self.sweep("POST", f"{box}/exec", {"command": []})
        self.sweep("GET", f"{box}/files?path=/gate/no/such/file")
        self.sweep("GET", f"{box}/files?path=relative")
        self.sweep("DELETE", f"/v0/snapshots/gate-no-such-{self.tag}")
        self.sweep("DELETE", f"{box}/ports/1")

    def cleanup(self) -> None:
        left = []
        for sid in self.sandboxes:
            try:
                self.actor("ts", "remove", sid)
                continue
            except Failed:
                pass
            got = self.request("DELETE", f"/v0/sandboxes/{sid}?force=true")
            if got.status >= 300 and got.status != 404:
                left.append(f"{sid} ({got.status})")
        for path in self.owned:
            got = self.request("DELETE", path)
            if got.status >= 300 and got.status != 404:
                left.append(f"{path} ({got.status})")
        if left:
            self.results["gate.cleanup"] = Verdict("FAIL", f"could not remove {', '.join(left)}")
            self.say(f"FAIL gate.cleanup: {self.results['gate.cleanup'].detail}")

    def claims(self) -> bool:
        ok = True
        for claim, proofs in CLAIMS.items():
            missing = [bad for proof in proofs for bad in self.unproven(proof)]
            ok = ok and not missing
            verdict = "PASS" if not missing else "FAIL"
            self.say(f"CLAIM {claim}: {verdict} ({', '.join(proofs) if not missing else ', '.join(missing)})")
        return ok

    def unproven(self, proof: str) -> Iterator[str]:
        if proof.startswith("suite:"):
            yield from self.unproven_suite(proof.removeprefix("suite:"))
            return
        if self.results.get(proof, Verdict("FAIL")).verdict != "PASS":
            yield proof

    def unproven_suite(self, pattern: str) -> Iterator[str]:
        """Every listed check the pattern names must pass, or skip where MAY_SKIP allows, in every run."""
        prefix = pattern.removesuffix("*")
        named = [name for name in self.listed if name == pattern or (pattern.endswith("*") and name.startswith(prefix))]
        if not named:
            yield f"sdks/suite/checks.txt names no {pattern}"
        for run in SUITES:
            yield from self.unproven_in(run, named)

    def unproven_in(self, run: str, names: list[str]) -> Iterator[str]:
        lines = self.suite_lines.get(run)
        if lines is None:
            yield f"{run} never ran"
            return
        for name in names:
            line = lines.get(name)
            if line is None:
                yield f"{run} ran no {name}"
                continue
            if line.verdict == "FAIL" or (line.verdict == "SKIP" and name not in MAY_SKIP):
                yield f"{run} {name}"


def keys(value: Any) -> Iterator[str]:
    if isinstance(value, dict):
        for key, inner in value.items():
            yield str(key)
            yield from keys(inner)
    if isinstance(value, list):
        for inner in value:
            yield from keys(inner)


def properties(spec: Any) -> set[str]:
    names: set[str] = set()
    if isinstance(spec, dict):
        if isinstance(spec.get("properties"), dict):
            names.update(spec["properties"])
        for inner in spec.values():
            names |= properties(inner)
    if isinstance(spec, list):
        for inner in spec:
            names |= properties(inner)
    return names


def objects(value: Any) -> Iterator[dict[str, Any]]:
    if isinstance(value, dict):
        yield value
        for inner in value.values():
            yield from objects(inner)
    if isinstance(value, list):
        for inner in value:
            yield from objects(inner)


def owned_properties(spec: Any) -> Iterator[tuple[str, str]]:
    """Each property name of the spec, beside the component schema it sits in, or "" outside every one."""
    components = spec.get("components", {})
    for owner, schema in components.get("schemas", {}).items():
        yield from ((owner, name) for name in sorted(properties(schema)))
    rest = {**spec, "components": {kind: each for kind, each in components.items() if kind != "schemas"}}
    yield from (("", name) for name in sorted(properties(rest)))


def route_pattern(template: str) -> re.Pattern[str]:
    return re.compile("/".join("[^/]+" if part.startswith("{") else re.escape(part) for part in template.split("/")))


def tar_of(name: str, data: bytes) -> bytes:
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode="w") as tar:
        info = tarfile.TarInfo(name)
        info.size = len(data)
        tar.addfile(info, io.BytesIO(data))
    return out.getvalue()


def free_port() -> int:
    """A port nothing on this host listens on now, which a daemon on this same host may then bind."""
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port: int = probe.getsockname()[1]
        return port


def host_fields() -> list[str]:
    return [line for line in (GATE / "host-fields.txt").read_text().splitlines() if line]


def local_routes() -> list[tuple[str, str]]:
    lines = (GATE / "local-routes.txt").read_text().splitlines()
    return [(method, pattern) for method, pattern in (line.split(" ", 1) for line in lines if line)]


def variants(path: str) -> list[str]:
    """The path, and spellings a front could normalize to it while matching another route."""
    rest = path.removeprefix("/v0/")
    return [
        path,
        path + "/",
        path + "?gate=1",
        f"/v0//{rest}",
        f"/v0/./{rest}",
        f"/v0/gate/../{rest}",
        path.upper(),
        f"/v0/%{ord(rest[0]):02X}{rest[1:]}",
    ]


def newest_capture() -> tuple[int, str]:
    data = b"".join(b"%09d\n" % i for i in range(CAPTURE_LINES))
    return len(data), hashlib.sha256(data[-CAPTURE_LIMIT:]).hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser(description="The release gate of both SDKs against one daemon.")
    parser.add_argument("--tree", type=Path, default=GATE.parents[1], help="the checkout whose SDKs and spec run")
    parser.add_argument("--out", type=Path, help="where the logs and recorded answers go; a temp dir by default")
    parser.add_argument("--only", default="", help="run only these checks, comma separated")
    args = parser.parse_args()
    for key in ("SHARD_REMOTE", "SHARD_API_KEY", "SHARD_SUITE_WILDCARD_KEY"):
        if not os.environ.get(key):
            print(f"gate: {key} is not set", file=sys.stderr)
            return 2
    out = args.out or Path(tempfile.mkdtemp(prefix="shard-gate-out-"))
    out.mkdir(parents=True, exist_ok=True)
    gate = Gate(args.tree.resolve(), out.resolve(), {name for name in args.only.split(",") if name})
    print(f"gate: tree {gate.tree}, logs in {gate.out}", file=sys.stderr, flush=True)

    try:
        gate.check("install.typescript", gate.install_typescript)
        gate.check("install.python", gate.install_python)
        gate.check("examples.typescript", gate.example_typescript)
        gate.check("examples.python_sync", lambda: gate.example_python("quickstart.py"))
        gate.check("examples.python_async", lambda: gate.example_python("quickstart_async.py"))
        for run in SUITES:
            gate.check(f"suite.{run}", functools.partial(gate.suite, run))
        gate.check("cross.nonzero_result", gate.nonzero)
        gate.check("cross.cancel_ts_seen_by_py", lambda: gate.cancel("ts", "py", "AbortError"))
        gate.check("cross.cancel_py_seen_by_ts", lambda: gate.cancel("py", "ts", "CancelledError"))
        gate.check("cross.reconnect_ts_to_py", lambda: gate.reconnect("ts", "py"))
        gate.check("cross.reconnect_py_to_ts", lambda: gate.reconnect("py", "ts"))
        gate.check("cross.files_ts_to_py", lambda: gate.files("ts", "py"))
        gate.check("cross.files_py_to_ts", lambda: gate.files("py", "ts"))
        want = newest_capture()
        for run in SUITES:
            gate.check(f"capture.last_8mib_{run}", functools.partial(gate.capture, run, want))
        gate.check("leak.spec", gate.leak_spec)
        gate.check("leak.local_routes", gate.leak_local_routes)
        gate.check("leak.responses", gate.leak_responses)
    finally:
        gate.cleanup()

    proven = bool(gate.only) or gate.claims()
    failed = [name for name, result in gate.results.items() if result.verdict == "FAIL"]
    print(f"gate: {len(gate.results)} checks, {len(failed)} failed; logs in {gate.out}", file=sys.stderr)
    return 1 if failed or not proven else 0


if __name__ == "__main__":
    sys.exit(main())
