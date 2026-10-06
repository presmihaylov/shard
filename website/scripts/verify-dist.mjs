// Checks the built site: every route exists, the search index was built, every internal link resolves, every page carries its SEO head, the landing page matches its design to the byte, and /install is the script.
import { createHash } from 'node:crypto';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

const dist = new URL('../dist/', import.meta.url).pathname;
const pub = new URL('../public/', import.meta.url).pathname;

// The landing page ships exactly as designed: change these hashes only together with a new design.
const landing = [
	['index.html', '49e656ece102cfc3db98e8f1014cc38f57b2773f2d8d919822ce29d74db5a2b2'],
	['prism-core.js', '6caad316dd991f24f8004e0b9c19c055cb5829ff65e973fbee406f96d81b8e7e'],
	['prism-clike.js', 'c76ba4e240932bdc75546be30e550f5ba5e13815ff71511c76e9e27ac3072444'],
	['prism-javascript.js', '0345ea83e12b7b974e953c79a64dea35a40308309449db70b82020fb688ac321'],
	['prism-python.js', 'ed4385685bcf2d4935c8dbbab4bde16603da1329e092d2bf36c3dadd67e9a85c'],
	['prism-typescript.js', '852f5513bb9ca9db247f86ecfce74acc91c541749d34929157240518fef8152a'],
	['logo.png', 'b440e19dc7fa7b34e0bdf7d14c09ba72fb35c1f34b08b77b71fe726afcbb681d'],
	['logo-simple.png', 'd202267a70e5008c2926348f09a9dcf55d9c99506ae2ba74b051547ae5a4e384'],
	['shard-reference-logo.png', '98c14085204b71ffab286453f49640c0382ddafd6cffe48ce64e67f6f8d1a330'],
	['single-shard.png', '8dd6a5993bc520bacc498169a66f6515216dd69cd89e972cf5927e1f659a1e75'],
	['fonts/geist-latin-wght-normal.woff2', '19f9c92546aa300c312235e3125af1b81394d8db9a4bc4a425cd5b641d2d54e1'],
	['fonts/gugi-latin-400-normal.woff2', 'ba4a17d21db19c2214ad6178e06f7c19b1aab881760367b33fd3b2a37c7a802c'],
	['fonts/jetbrains-mono-latin-wght-normal.woff2', '18be452724bfdc236c074ca94a249a7f41a86752c7d04ab258ce9ed5651f6a7e'],
	['fonts/outfit-latin-wght-normal.woff2', '6c18d579fd87c3776be068b762cbc83fde3acb543d49eabd3ade842eb987e887'],
	['fonts/space-grotesk-latin-wght-normal.woff2', '0640890476fc1198ab4de571fb658de443c4d85b66466ec09534a8737ab1ce9d'],
];

const requiredFiles = [
	'404.html',
	'docs/index.html',
	'docs/install/index.html',
	'pagefind/pagefind.js',
	'shard-mark.svg',
	'install',
	'robots.txt',
	'sitemap.xml',
	'sitemap-index.xml',
	'llms.txt',
	'llms-full.txt',
	'docs.md',
	'docs/install.md',
	'favicon.ico',
	'icon.png',
	'apple-touch-icon.png',
	'og/index.png',
	...landing.map(([file]) => file),
];

function htmlFiles(dir) {
	return readdirSync(dir).flatMap((name) => {
		const path = join(dir, name);
		if (statSync(path).isDirectory()) return htmlFiles(path);
		return name.endsWith('.html') ? [path] : [];
	});
}

function resolves(href) {
	const path = decodeURI(href.split(/[?#]/)[0]);
	const candidates = [path, join(path, 'index.html'), `${path}.html`];
	return candidates.some((candidate) => {
		const full = join(dist, candidate);
		return existsSync(full) && statSync(full).isFile();
	});
}

function textFiles(dir) {
	return readdirSync(dir).flatMap((name) => {
		const path = join(dir, name);
		if (statSync(path).isDirectory()) return textFiles(path);
		return /\.(html|md|txt|xml)$/.test(name) ? [path] : [];
	});
}

const problems = requiredFiles.filter((file) => !existsSync(join(dist, file))).map((file) => `missing ${file}`);

for (const file of htmlFiles(dist)) {
	const html = readFileSync(file, 'utf8');
	for (const [, href] of html.matchAll(/(?:href|src)="(\/[^"]*)"/g)) {
		if (href.startsWith('//') || resolves(href)) continue;
		problems.push(`${relative(dist, file)}: broken link ${href}`);
	}
}

// The sitemaps, the llms files, the Markdown twins and the meta tags name pages by their full URL.
for (const file of textFiles(dist)) {
	const text = readFileSync(file, 'utf8');
	for (const [, path] of text.matchAll(/https:\/\/useshards\.com(\/[^\s"'<>)`\]]*)?/g)) {
		if (resolves(path ?? '/')) continue;
		problems.push(`${relative(dist, file)}: broken link https://useshards.com${path ?? ''}`);
	}
}

const titles = new Map();
const descriptions = new Map();
for (const file of htmlFiles(dist)) {
	const name = relative(dist, file);
	if (name === '404.html' || name.startsWith('pagefind/')) continue;
	const html = readFileSync(file, 'utf8');
	const head = html.split('</head>')[0];
	const body = html.slice(head.length);
	const has = (pattern) => pattern.test(head);
	const title = /<title>([^<]*)<\/title>/.exec(head)?.[1];
	if (!title) problems.push(`${name}: no title`);
	if (title && titles.has(title)) problems.push(`${name}: title "${title}" also on ${titles.get(title)}`);
	titles.set(title, name);
	const description = /<meta name="description" content="([^"]+)"/.exec(head)?.[1];
	if (!description) problems.push(`${name}: no meta description`);
	if (description && descriptions.has(description)) problems.push(`${name}: description also on ${descriptions.get(description)}`);
	descriptions.set(description, name);
	if (!has(/<link rel="canonical" href="https:\/\/useshards\.com\//)) problems.push(`${name}: no canonical`);
	if (!has(/<meta name="robots" content="index, follow"/)) problems.push(`${name}: not index, follow`);
	for (const tag of ['og:title', 'og:description', 'og:url', 'og:site_name', 'og:locale', 'og:type', 'og:image', 'og:image:alt']) {
		if (!has(new RegExp(`<meta property="${tag}" content="[^"]+"`))) problems.push(`${name}: no ${tag}`);
	}
	for (const tag of ['twitter:card', 'twitter:title', 'twitter:description', 'twitter:image', 'twitter:image:alt']) {
		if (!has(new RegExp(`<meta name="${tag}" content="[^"]+"`))) problems.push(`${name}: no ${tag}`);
	}
	if (!has(/<script type="application\/ld\+json">/)) problems.push(`${name}: no JSON-LD`);
	const h1s = body.match(/<h1[\s>]/g)?.length ?? 0;
	if (h1s !== 1) problems.push(`${name}: ${h1s} h1 elements, want 1`);
	for (const [img] of body.matchAll(/<img\b[^>]*>/g)) {
		if (!/\salt="/.test(img)) problems.push(`${name}: an img with no alt`);
	}
}

// The landing OG image renders from src/seo.ts, so its title and description must match the static landing.
const landingHead = readFileSync(join(pub, 'index.html'), 'utf8').split('</head>')[0];
const seo = readFileSync(new URL('../src/seo.ts', import.meta.url), 'utf8');
for (const pattern of [/<title>([^<]*)<\/title>/, /<meta name="description" content="([^"]*)"/]) {
	const value = pattern.exec(landingHead)?.[1];
	if (!value || !seo.includes(`'${value}'`)) problems.push(`src/seo.ts: LANDING does not match the landing's ${value}`);
}

const sha256 = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
for (const [file, want] of landing) {
	const path = join(dist, file);
	if (existsSync(path) && sha256(path) !== want) problems.push(`${file}: not byte-identical to the landing design`);
}

const install = join(dist, 'install');
if (existsSync(install) && !readFileSync(install, 'utf8').startsWith('#!/bin/sh\n')) problems.push('install: not the shell script');
if (existsSync(install) && sha256(install) !== sha256(join(pub, 'install'))) problems.push('install: differs from public/install');

if (problems.length > 0) {
	console.error(problems.join('\n'));
	process.exit(1);
}
console.log(`dist ok: ${requiredFiles.length} required files, ${landing.length} landing files byte-identical, every internal link resolves, ${titles.size} pages carry their SEO head`);
