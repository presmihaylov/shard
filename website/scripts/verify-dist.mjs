// Checks the built site: every route exists, the search index was built, and every internal link resolves.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

const dist = new URL('../dist/', import.meta.url).pathname;

const requiredFiles = [
	'index.html',
	'404.html',
	'docs/index.html',
	'docs/components/index.html',
	'docs/guides/placeholder/index.html',
	'pagefind/pagefind.js',
	'favicon.svg',
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

const problems = requiredFiles.filter((file) => !existsSync(join(dist, file))).map((file) => `missing ${file}`);

for (const file of htmlFiles(dist)) {
	const html = readFileSync(file, 'utf8');
	for (const [, href] of html.matchAll(/(?:href|src)="(\/[^"]*)"/g)) {
		if (href.startsWith('//') || resolves(href)) continue;
		problems.push(`${relative(dist, file)}: broken link ${href}`);
	}
}

if (problems.length > 0) {
	console.error(problems.join('\n'));
	process.exit(1);
}
console.log(`dist ok: ${requiredFiles.length} required files, every internal link resolves`);
