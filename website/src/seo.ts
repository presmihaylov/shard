import { type CollectionEntry, getCollection } from 'astro:content';
import { REPO_URL } from './site';

export const SITE = 'https://useshards.com';
export const SITE_NAME = 'shard';
export const INSTALL = `curl -fsSL ${SITE}/install | sh`;

// public/index.html is static, so its head repeats these; scripts/verify-dist.mjs checks that the two agree.
export const LANDING = {
	title: 'Shard · Sandboxes for AI agents',
	description: 'Give your agents sandboxes on hardware you control, with one API across different runtimes.',
};

export const ORGANIZATION_ID = `${SITE}/#organization`;
export const WEBSITE_ID = `${SITE}/#website`;

export type DocsEntry = CollectionEntry<'docs'>;

export const docsPages = async () => (await getCollection('docs')).filter((entry) => entry.id !== '404');

export const pageUrl = (id: string) => `${SITE}/${id}/`;
export const markdownUrl = (id: string) => `${SITE}/${id}.md`;
export const ogImageUrl = (id: string) => `${SITE}/og/${id}.png`;

// The sidebar groups, in sidebar order; a page joins the first group whose prefix its id starts with.
const GROUPS: [label: string, prefixes: string[]][] = [
	['Get started', ['docs/install', 'docs/quickstart/', 'docs/choose-provider']],
	['Concepts', ['docs/concepts/']],
	['Security', ['docs/security/']],
	['Guides', ['docs/guides/']],
	['SDKs', ['docs/sdks/']],
	['CLI reference', ['docs/reference/cli']],
	['REST API reference', ['docs/reference/api']],
	['Reference', ['docs/reference/']],
	['Help', ['docs/help/']],
];

const groupOf = (id: string) => (id === 'docs' ? 0 : GROUPS.findIndex(([, prefixes]) => prefixes.some((prefix) => id.startsWith(prefix))));
// Within a group the prefixes keep the sidebar's order; the docs home matches none, so it leads Get started.
const prefixOf = (id: string) => GROUPS[groupOf(id)]?.[1].findIndex((prefix) => id.startsWith(prefix)) ?? -1;
const orderOf = (entry: DocsEntry) => entry.data.sidebar.order ?? Number.POSITIVE_INFINITY;

export const groupedDocs = async () => {
	const ordered = (await docsPages()).sort(
		(a, b) =>
			groupOf(a.id) - groupOf(b.id) || prefixOf(a.id) - prefixOf(b.id) || orderOf(a) - orderOf(b) || a.id.localeCompare(b.id),
	);
	return GROUPS.map(([label], index) => ({ label, pages: ordered.filter((entry) => groupOf(entry.id) === index) })).filter(
		(group) => group.pages.length > 0,
	);
};

// Pages that share a title, like the CLI and REST API pages of one resource, take their group's name, so every document title stays unique.
export const pageTitles = async () => {
	const pages = await docsPages();
	const count = (title: string) => pages.filter((entry) => entry.data.title === title).length;
	return new Map(
		pages.map((entry) => {
			const { title } = entry.data;
			return [entry.id, count(title) > 1 ? `${title} · ${GROUPS[groupOf(entry.id)]?.[0]}` : title];
		}),
	);
};

// A breadcrumb names an index page by its sidebar group, since its own title is "Overview".
const CRUMBS: Record<string, string> = { docs: 'Docs', 'docs/reference/cli': 'CLI', 'docs/reference/api': 'REST API' };

export const breadcrumbs = (id: string, title: string, ids: Set<string>) => {
	const parts = id.split('/');
	const parents = parts.slice(1).map((_, index) => parts.slice(0, index + 1).join('/'));
	const crumbs = parents.filter((parent) => ids.has(parent)).map((parent) => ({ name: CRUMBS[parent] ?? parent, url: pageUrl(parent) }));
	return [{ name: SITE_NAME, url: `${SITE}/` }, ...crumbs, { name: CRUMBS[id] ?? title, url: pageUrl(id) }];
};

export const organization = {
	'@type': 'Organization',
	'@id': ORGANIZATION_ID,
	name: SITE_NAME,
	url: `${SITE}/`,
	logo: { '@type': 'ImageObject', url: `${SITE}/icon.png`, width: 512, height: 512 },
	sameAs: [REPO_URL],
};

// JSON in a script tag must not close it early.
export const jsonLd = (graph: object[]) => JSON.stringify({ '@context': 'https://schema.org', '@graph': graph }).replaceAll('<', '\\u003c');

const absolute = (markdown: string) => markdown.replace(/\]\(\/(?!\/)/g, `](${SITE}/`).replace(/href="\/(?!\/)/g, `href="${SITE}/`);

// The MDX the docs use beyond Markdown is Tabs, TabItem, LinkCard and one-line comments; each becomes plain Markdown.
const plain = (body: string) => {
	const out: string[] = [];
	let fence = '';
	for (const line of body.split('\n')) {
		const marker = /^\s*(`{3,}|~{3,})/.exec(line)?.[1];
		if (marker && (!fence || marker.startsWith(fence))) {
			fence = fence ? '' : marker;
			out.push(line);
			continue;
		}
		if (fence) {
			out.push(line);
			continue;
		}
		if (/^import\s.+\sfrom\s/.test(line) || /^\s*<\/?Tabs\b[^>]*>\s*$/.test(line) || /^\s*<\/TabItem>\s*$/.test(line)) continue;
		const tab = /^\s*<TabItem\s+label="([^"]+)"[^>]*>\s*$/.exec(line);
		if (tab) {
			out.push(`**${tab[1]}**`);
			continue;
		}
		const card = /^\s*<LinkCard\s+(.*)\/>\s*$/.exec(line);
		if (card) {
			const attr = (name: string) => new RegExp(`${name}="([^"]*)"`).exec(card[1])?.[1] ?? '';
			out.push(`- [${attr('title')}](${attr('href')}): ${attr('description')}`);
			continue;
		}
		out.push(line.replace(/\{\/\*.*?\*\/\}/g, ''));
	}
	return absolute(out.join('\n'))
		.replace(/\n{3,}/g, '\n\n')
		.trim();
};

export const toMarkdown = (entry: DocsEntry) =>
	`# ${entry.data.title}\n\n> ${entry.data.description ?? ''}\n\nSource: ${pageUrl(entry.id)}\n\n${plain(entry.body ?? '')}\n`;
