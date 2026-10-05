// Runs public/install against a fake GitHub on localhost, with a fake HOME, uname and sysctl. It never touches the real host.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, before, describe, test } from 'node:test';
import { fileURLToPath } from 'node:url';

const scriptPath = fileURLToPath(new URL('../public/install', import.meta.url));
const script = readFileSync(scriptPath, 'utf8');
const which = (name) => spawnSync('sh', ['-c', `command -v ${name}`], { encoding: 'utf8' }).stdout.trim();
const realCurl = which('curl');
const python = which('python3');
const shells = [...new Map(['sh', 'dash', 'bash'].filter(which).map((name) => [realpathSync(which(name)), name])).values()];

const root = mkdtempSync(join(tmpdir(), 'shard-install-test-'));
const fakebin = join(root, 'bin');
const state = { releases: null, files: new Map(), hang: null, onHang: null, requests: [] };
const server = createServer((req, res) => {
	state.requests.push(req.url);
	if (req.url.startsWith('/api/repos/presmihaylov/shard/releases')) {
		res.writeHead(state.releases.status, state.releases.headers);
		res.end(state.releases.body);
		return;
	}
	const name = req.url.replace(/^\/gh\/presmihaylov\/shard\/releases\/download\//, '');
	if (name === state.hang) {
		state.onHang();
		return;
	}
	if (!state.files.has(name)) {
		res.writeHead(404);
		res.end('Not Found');
		return;
	}
	res.writeHead(200);
	res.end(state.files.get(name));
});
let base = '';

function executable(path, body) {
	writeFileSync(path, body);
	chmodSync(path, 0o755);
}

before(async () => {
	await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
	base = `http://127.0.0.1:${server.address().port}`;
	mkdirSync(fakebin);
	executable(
		join(fakebin, 'curl'),
		`#!/bin/sh
for arg do
	shift
	case "$arg" in
	https://api.github.com/*) arg="$FAKE_BASE/api/\${arg#https://api.github.com/}" ;;
	https://github.com/*) arg="$FAKE_BASE/gh/\${arg#https://github.com/}" ;;
	esac
	set -- "$@" "$arg"
done
exec "${realCurl}" "$@"
`,
	);
	executable(join(fakebin, 'uname'), '#!/bin/sh\ncase "$1" in -s) echo "$FAKE_OS" ;; -m) echo "$FAKE_ARCH" ;; esac\n');
	executable(join(fakebin, 'sysctl'), '#!/bin/sh\n[ -n "${FAKE_TRANSLATED:-}" ] || exit 1\necho "$FAKE_TRANSLATED"\n');
});

after(() => {
	server.closeAllConnections();
	server.close();
	rmSync(root, { recursive: true, force: true });
});

function fakeShard(version) {
	return `#!/bin/sh
case "$1" in
--version) echo "client ${version}" ;;
setup) if [ -t 0 ]; then echo tty >"$HOME/setup-ran"; else echo pipe >"$HOME/setup-ran"; fi ;;
esac
`;
}

const sha = (body) => createHash('sha256').update(body).digest('hex');

function release(tag, flags = {}) {
	return {
		url: `https://api.github.com/repos/presmihaylov/shard/releases/${tag}`,
		author: { login: 'u_789', id: 1, site_admin: false },
		tag_name: tag,
		target_commitish: 'main',
		name: tag,
		draft: flags.draft ?? false,
		prerelease: flags.prerelease ?? false,
		assets: [{ name: 'SHA256SUMS', uploader: { login: 'u_789' }, state: 'uploaded' }],
		body: 'Notes that quote "tag_name": "v9.9.9" and "draft": false.',
	};
}

function serveReleases(list, pretty = true) {
	state.releases = { status: 200, headers: { 'content-type': 'application/json' }, body: JSON.stringify(list, null, pretty ? 2 : 0) };
}

// Publishes one release with the given binaries, plus the files every real release carries.
function publish(tag, binaries) {
	const files = { ...binaries, 'shard-init-linux-amd64': 'init' };
	const sums = Object.entries(files).map(([name, body]) => `${sha(body)}  ${name}\n`);
	for (const [name, body] of Object.entries(files)) state.files.set(`${tag}/${name}`, body);
	state.files.set(`${tag}/SHA256SUMS`, sums.join(''));
}

function reset() {
	state.files.clear();
	state.requests.length = 0;
	state.hang = null;
	serveReleases([release('sdk-typescript-v0.1.0'), release('v0.2.0'), release('kernel-6.12.110-3')]);
	publish('v0.2.0', {
		'shard-linux-amd64': fakeShard('v0.2.0'),
		'shard-darwin-arm64': fakeShard('v0.2.0'),
		'shard-darwin-amd64': fakeShard('v0.2.0'),
	});
}

function sandbox() {
	const dir = mkdtempSync(join(root, 'run-'));
	mkdirSync(join(dir, 'home'));
	mkdirSync(join(dir, 'tmp'));
	return { home: join(dir, 'home'), tmp: join(dir, 'tmp'), bin: join(dir, 'home', '.local', 'bin') };
}

function environment(box, opts) {
	return {
		PATH: [fakebin, ...(opts.path ?? []), '/usr/bin', '/bin', '/usr/sbin', '/sbin'].join(':'),
		HOME: box.home,
		TMPDIR: box.tmp,
		SHELL: opts.shell ?? '/bin/zsh',
		FAKE_BASE: opts.base ?? base,
		FAKE_OS: opts.os ?? 'Linux',
		FAKE_ARCH: opts.arch ?? 'x86_64',
		...(opts.translated ? { FAKE_TRANSLATED: opts.translated } : {}),
		...(opts.env ?? {}),
	};
}

// Pipes the script into the shell as `curl ... | sh` does, in its own session, so it has no terminal.
function install(shell, box, opts = {}) {
	const child = spawn(shell, [], { env: environment(box, opts), detached: true, stdio: ['pipe', 'pipe', 'pipe'] });
	let stdout = '';
	let stderr = '';
	child.stdout.on('data', (chunk) => (stdout += chunk));
	child.stderr.on('data', (chunk) => (stderr += chunk));
	child.stdin.end(script);
	const done = new Promise((resolve) => child.on('close', (code, signal) => resolve({ code, signal, stdout, stderr })));
	return { child, done };
}

const downloads = () => state.requests.filter((url) => url.startsWith('/gh/'));
const leftovers = (box) => [...readdirSync(box.tmp), ...(existsSync(box.bin) ? readdirSync(box.bin).filter((name) => name !== 'shard') : [])];

for (const shell of shells) {
	describe(`install under ${shell}`, () => {
		test('picks the highest stable vX.Y.Z and downloads only from that release', async () => {
			reset();
			serveReleases([
				release('sdk-typescript-v0.3.0'),
				release('sdk-python-v0.3.0'),
				release('kernel-6.12.110-3'),
				release('v0.4.0', { prerelease: true }),
				release('v0.3.0-rc.1', { prerelease: true }),
				release('v1.0.0', { draft: true }),
				release('v0.1.10'),
				release('v0.2.0'),
				release('v0.1.9'),
				release('v1.0'),
			]);
			const box = sandbox();
			const result = await install(shell, box).done;
			assert.equal(result.code, 0, result.stderr);
			assert.ok(state.requests.includes('/api/repos/presmihaylov/shard/releases?per_page=100'));
			assert.deepEqual(downloads(), [
				'/gh/presmihaylov/shard/releases/download/v0.2.0/SHA256SUMS',
				'/gh/presmihaylov/shard/releases/download/v0.2.0/shard-linux-amd64',
			]);
			assert.match(result.stdout, /Downloading Shard v0\.2\.0 for linux-amd64\.\.\./);
		});

		test('reads a compact release list too', async () => {
			reset();
			serveReleases([release('v0.1.9'), release('v0.2.0'), release('sdk-python-v0.9.0')], false);
			const result = await install(shell, sandbox()).done;
			assert.equal(result.code, 0, result.stderr);
			assert.match(result.stdout, /Downloading Shard v0\.2\.0 /);
		});

		test('installs without a terminal: plain banner, no prompt, setup instructions, no leftovers', async () => {
			reset();
			const box = sandbox();
			const result = await install(shell, box).done;
			assert.equal(result.code, 0, result.stderr);
			assert.ok(!result.stdout.includes('\x1b'), 'no color without a terminal');
			assert.ok(result.stdout.startsWith('███████ ██   ██  █████  ██████  ██████\n'));
			assert.ok(result.stdout.includes('Installing the Shard CLI...\n'));
			assert.ok(result.stdout.includes(`✓ Shard installed\n\nInstalled at: ${box.bin}/shard\n\n`));
			assert.ok(result.stdout.endsWith('Set up Shard later:\n  shard setup\n\nFor automated setup:\n  shard setup --help\n'));
			assert.ok(!result.stdout.includes('Start the setup wizard?'));
			assert.equal(readFileSync(join(box.bin, 'shard'), 'utf8'), fakeShard('v0.2.0'));
			assert.equal(spawnSync(join(box.bin, 'shard'), ['--version'], { encoding: 'utf8' }).stdout, 'client v0.2.0\n');
			assert.ok(!existsSync(join(box.home, 'setup-ran')));
			assert.deepEqual(leftovers(box), []);
		});

		test('names the rc file when ~/.local/bin is not on PATH, and stays quiet when it is', async () => {
			reset();
			const zsh = await install(shell, sandbox(), { shell: '/bin/zsh' }).done;
			assert.ok(zsh.stdout.includes(`~/.local/bin is not on your PATH. Add it with:\n  echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc\n\nThat takes effect in a new shell. For this shell, run:\n  export PATH="$HOME/.local/bin:$PATH"\n\n`));
			const fish = await install(shell, sandbox(), { shell: '/usr/bin/fish' }).done;
			assert.ok(fish.stdout.includes('fish_add_path ~/.local/bin'));
			assert.ok(!fish.stdout.includes('For this shell'));
			const box = sandbox();
			const onPath = await install(shell, box, { path: [box.bin] }).done;
			assert.equal(onPath.code, 0, onPath.stderr);
			assert.ok(!onPath.stdout.includes('not on your PATH'));
		});

		test('replaces an existing binary once the new one is verified', async () => {
			reset();
			const box = sandbox();
			mkdirSync(box.bin, { recursive: true });
			executable(join(box.bin, 'shard'), fakeShard('v0.1.0'));
			const result = await install(shell, box).done;
			assert.equal(result.code, 0, result.stderr);
			assert.equal(readFileSync(join(box.bin, 'shard'), 'utf8'), fakeShard('v0.2.0'));
			assert.deepEqual(leftovers(box), []);
		});

		test('Apple silicon under Rosetta gets the arm64 build', async () => {
			reset();
			const result = await install(shell, sandbox(), { os: 'Darwin', arch: 'x86_64', translated: '1' }).done;
			assert.equal(result.code, 0, result.stderr);
			assert.ok(downloads().includes('/gh/presmihaylov/shard/releases/download/v0.2.0/shard-darwin-arm64'));
			assert.ok(!result.stdout.includes('Intel Mac'));
		});

		test('an Intel Mac gets the amd64 build and the remote-only note', async () => {
			reset();
			const result = await install(shell, sandbox(), { os: 'Darwin', arch: 'x86_64' }).done;
			assert.equal(result.code, 0, result.stderr);
			assert.ok(downloads().includes('/gh/presmihaylov/shard/releases/download/v0.2.0/shard-darwin-amd64'));
			assert.ok(result.stdout.includes('An Intel Mac can connect to a remote Shard server. It cannot host sandboxes itself.'));
		});

		test('a platform the release has no build for fails before the binary download', async () => {
			reset();
			const box = sandbox();
			const result = await install(shell, box, { arch: 'aarch64' }).done;
			assert.equal(result.code, 1);
			assert.ok(result.stderr.includes('Shard v0.2.0 has no build for linux-arm64. Its builds are: linux-amd64, darwin-arm64, darwin-amd64.'));
			assert.deepEqual(downloads(), ['/gh/presmihaylov/shard/releases/download/v0.2.0/SHA256SUMS']);
			assert.ok(!existsSync(box.bin));
			assert.deepEqual(leftovers(box), []);
		});

		test('an unsupported system fails before any request', async () => {
			reset();
			const bsd = await install(shell, sandbox(), { os: 'FreeBSD', arch: 'amd64' }).done;
			assert.equal(bsd.code, 1);
			assert.ok(bsd.stderr.includes('Shard runs on Linux and macOS, and this system is FreeBSD.'));
			const windows = await install(shell, sandbox(), { os: 'MINGW64_NT-10.0' }).done;
			assert.ok(windows.stderr.includes('Run this installer inside WSL 2.'));
			assert.deepEqual(state.requests, []);
		});

		test('a checksum mismatch installs nothing and keeps the old binary', async () => {
			reset();
			state.files.set('v0.2.0/shard-linux-amd64', fakeShard('v0.2.0') + '# tampered\n');
			const box = sandbox();
			mkdirSync(box.bin, { recursive: true });
			executable(join(box.bin, 'shard'), fakeShard('v0.1.0'));
			const result = await install(shell, box).done;
			assert.equal(result.code, 1);
			assert.match(result.stderr, /the checksum of shard-linux-amd64 is [0-9a-f]{64}, and SHA256SUMS of Shard v0\.2\.0 says [0-9a-f]{64}\. Nothing was installed\./);
			assert.equal(readFileSync(join(box.bin, 'shard'), 'utf8'), fakeShard('v0.1.0'));
			assert.deepEqual(leftovers(box), []);
		});

		test('a binary that reports another version installs nothing and keeps the old binary', async () => {
			reset();
			publish('v0.2.0', { 'shard-linux-amd64': fakeShard('v0.1.5') });
			const box = sandbox();
			mkdirSync(box.bin, { recursive: true });
			executable(join(box.bin, 'shard'), fakeShard('v0.1.0'));
			const result = await install(shell, box).done;
			assert.equal(result.code, 1);
			assert.ok(result.stderr.includes('the downloaded binary does not report version v0.2.0. Nothing was installed.'));
			assert.equal(readFileSync(join(box.bin, 'shard'), 'utf8'), fakeShard('v0.1.0'));
			assert.deepEqual(leftovers(box), []);
		});

		test('a missing asset fails and cleans up', async () => {
			reset();
			state.files.delete('v0.2.0/shard-linux-amd64');
			const box = sandbox();
			const result = await install(shell, box).done;
			assert.equal(result.code, 1);
			assert.ok(result.stderr.includes('could not download shard-linux-amd64 from Shard v0.2.0.'));
			assert.deepEqual(leftovers(box), []);
		});

		test('a spent API rate limit fails with its cause and never falls back to a fixed version', async () => {
			reset();
			state.releases = { status: 403, headers: { 'x-ratelimit-remaining': '0' }, body: '{"message":"API rate limit exceeded"}' };
			const box = sandbox();
			const result = await install(shell, box).done;
			assert.equal(result.code, 1);
			assert.ok(result.stderr.includes('the GitHub releases API rate limit for this IP address is spent (HTTP 403).'));
			assert.deepEqual(downloads(), []);
			assert.deepEqual(leftovers(box), []);
		});

		test('other API failures name the status or the network', async () => {
			reset();
			state.releases = { status: 500, headers: {}, body: 'oops' };
			const failed = await install(shell, sandbox()).done;
			assert.ok(failed.stderr.includes('the GitHub releases API returned HTTP 500.'));
			state.releases = { status: 403, headers: { 'x-ratelimit-remaining': '12' }, body: '{}' };
			const refused = await install(shell, sandbox()).done;
			assert.ok(refused.stderr.includes('the GitHub releases API refused the request (HTTP 403).'));
			const offline = await install(shell, sandbox(), { base: 'http://127.0.0.1:1' }).done;
			assert.equal(offline.code, 1);
			assert.ok(offline.stderr.includes('could not reach the GitHub releases API at api.github.com.'));
			assert.deepEqual(downloads(), []);
		});

		test('a list with no stable shard release fails', async () => {
			reset();
			serveReleases([release('sdk-python-v0.1.0'), release('v0.3.0-rc.1', { prerelease: true }), release('kernel-6.12.110-3')]);
			const result = await install(shell, sandbox()).done;
			assert.equal(result.code, 1);
			assert.ok(result.stderr.includes('found no published Shard release on GitHub.'));
		});

		test('an interrupt mid-download removes the temp files and keeps the old binary', async () => {
			reset();
			state.hang = 'v0.2.0/shard-linux-amd64';
			const box = sandbox();
			mkdirSync(box.bin, { recursive: true });
			executable(join(box.bin, 'shard'), fakeShard('v0.1.0'));
			const hung = new Promise((resolve) => (state.onHang = resolve));
			const run = install(shell, box);
			await hung;
			assert.equal(readdirSync(box.tmp).length, 1, 'the temp dir exists mid-download');
			process.kill(-run.child.pid, 'SIGINT');
			const result = await run.done;
			assert.equal(result.code, 130);
			assert.equal(readFileSync(join(box.bin, 'shard'), 'utf8'), fakeShard('v0.1.0'));
			assert.deepEqual(leftovers(box), []);
		});
	});
}

// Runs `cat install | sh` on a pseudo-terminal, answers the setup prompt, and prints what the terminal showed.
const ptyRunner = `
import os, pty, sys
answer = sys.argv[2].encode()
pid, fd = pty.fork()
if pid == 0:
    os.execvp("sh", ["sh", "-c", 'cat "$0" | sh', sys.argv[1]])
out = b""
answered = False
while True:
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    out += chunk
    if not answered and b"[Y/n] " in out:
        os.write(fd, answer + b"\\n")
        answered = True
_, status = os.waitpid(pid, 0)
sys.stdout.buffer.write(out)
sys.exit(os.waitstatus_to_exitcode(status))
`;

function installOnTerminal(box, answer, opts = {}) {
	const child = spawn(python, ['-c', ptyRunner, scriptPath, answer], { env: environment(box, opts), stdio: ['ignore', 'pipe', 'pipe'] });
	let stdout = '';
	let stderr = '';
	child.stdout.on('data', (chunk) => (stdout += chunk));
	child.stderr.on('data', (chunk) => (stderr += chunk));
	return new Promise((resolve) => child.on('close', (code) => resolve({ code, stdout: stdout.replaceAll('\r\n', '\n'), stderr })));
}

describe('install on a terminal', { skip: python === '' && 'needs python3 for a pseudo-terminal' }, () => {
	test('Enter launches shard setup by its absolute path, with the terminal as its input', async () => {
		reset();
		const box = sandbox();
		const result = await installOnTerminal(box, '');
		assert.equal(result.code, 0, result.stdout + result.stderr);
		assert.ok(result.stdout.includes('Start the setup wizard? [Y/n] '));
		assert.equal(readFileSync(join(box.home, 'setup-ran'), 'utf8'), 'tty\n');
		assert.ok(!result.stdout.includes('Set up Shard later:'));
		assert.deepEqual(leftovers(box), []);
	});

	test('n declines and prints the setup instructions', async () => {
		reset();
		const box = sandbox();
		const result = await installOnTerminal(box, 'n');
		assert.equal(result.code, 0, result.stdout + result.stderr);
		assert.ok(!existsSync(join(box.home, 'setup-ran')));
		assert.ok(result.stdout.endsWith('Set up Shard later:\n  shard setup\n\nFor automated setup:\n  shard setup --help\n'));
	});

	test('the banner runs #585BE2 to #3E57DE to #93A8EF on a truecolor terminal', async () => {
		reset();
		const result = await installOnTerminal(sandbox(), 'n', { env: { COLORTERM: 'truecolor' } });
		for (const rgb of ['88;91;226', '87;91;226', '62;87;222', '66;91;223', '147;168;239']) {
			assert.ok(result.stdout.includes(`\x1b[38;2;${rgb}m█`), rgb);
		}
		assert.ok(result.stdout.includes('\x1b[38;2;110;214;175m✓\x1b[0m Shard installed'));
	});

	test('a terminal without truecolor gets 256-color codes', async () => {
		reset();
		const result = await installOnTerminal(sandbox(), 'n');
		assert.ok(result.stdout.includes('\x1b[38;5;62m█'));
		assert.ok(!result.stdout.includes('\x1b[38;2;'));
	});

	test('NO_COLOR keeps the terminal output plain', async () => {
		reset();
		const result = await installOnTerminal(sandbox(), 'n', { env: { NO_COLOR: '1', COLORTERM: 'truecolor' } });
		assert.ok(!result.stdout.includes('\x1b'));
		assert.ok(result.stdout.includes('✓ Shard installed'));
	});
});
