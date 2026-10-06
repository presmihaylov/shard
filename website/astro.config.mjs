// @ts-check
import react from '@astrojs/react';
import sitemap from '@astrojs/sitemap';
import starlight from '@astrojs/starlight';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'astro/config';
import { existsSync, readdirSync, readFileSync } from 'node:fs';

const docs = new URL('./src/content/docs/', import.meta.url);
const isPage = (/** @type {string} */ name) => /\.mdx?$/.test(name);

// A page or a group enters the sidebar once its file exists, so a page PR only adds a file.
const pages = (/** @type {string[]} */ ...slugs) =>
	slugs
		.filter((slug) => [`${slug}.md`, `${slug}.mdx`, `${slug}/index.mdx`].some((file) => existsSync(new URL(file, docs))))
		.map((slug) => ({ slug }));
const dir = (/** @type {string} */ directory) => {
	const path = new URL(`${directory}/`, docs);
	if (!existsSync(path) || !readdirSync(path, { recursive: true }).some((name) => isPage(String(name)))) return [];
	return [{ autogenerate: { directory } }];
};
const group = (/** @type {string} */ label, /** @type {string} */ directory) => {
	const items = dir(directory);
	return items.length > 0 ? [{ label, collapsed: true, items }] : [];
};
const order = (/** @type {URL} */ file) => Number(/^[ \t]+order:[ \t]*(-?\d+)/m.exec(readFileSync(file, 'utf8').split(/^---$/m)[1] ?? '')?.[1] ?? Infinity);
// The top-level reference pages share their group with the CLI and REST API sub-groups, so they are listed here by sidebar.order.
const files = (/** @type {string} */ directory) => {
	const path = new URL(`${directory}/`, docs);
	if (!existsSync(path)) return [];
	return readdirSync(path)
		.filter(isPage)
		.map((name) => ({ name, rank: order(new URL(name, path)) }))
		.sort((a, b) => a.rank - b.rank || a.name.localeCompare(b.name))
		.map(({ name }) => ({ slug: `${directory}/${name.replace(/\.mdx?$/, '')}` }));
};
const sidebar = [
	{ label: 'Get started', items: [...pages('docs', 'docs/install'), ...dir('docs/quickstart'), ...pages('docs/choose-provider')] },
	{ label: 'Concepts', items: dir('docs/concepts') },
	{ label: 'Security', items: dir('docs/security') },
	{ label: 'Guides', items: dir('docs/guides') },
	{ label: 'SDKs', items: dir('docs/sdks') },
	{ label: 'Reference', items: [...group('CLI', 'docs/reference/cli'), ...group('REST API', 'docs/reference/api'), ...files('docs/reference')] },
	{ label: 'Help', items: dir('docs/help') },
].filter((entry) => entry.items.length > 0);

// The landing's code panel and its Prism token colors, as a Shiki theme.
const terminal = {
	name: 'shard-terminal',
	type: 'dark',
	colors: {
		'editor.background': '#030b13',
		'editor.foreground': '#edf7ff',
		'editor.selectionBackground': '#2D81BE66',
	},
	tokenColors: [
		{ scope: ['comment', 'punctuation.definition.comment'], settings: { foreground: '#9eb9ce' } },
		{ scope: ['keyword', 'storage', 'storage.type', 'storage.modifier'], settings: { foreground: '#91C4ED' } },
		{ scope: ['string', 'string.quoted', 'punctuation.definition.string'], settings: { foreground: '#efe4dc' } },
		{ scope: ['entity.name.function', 'support.function', 'meta.function-call'], settings: { foreground: '#d0f0ff' } },
		{ scope: ['constant.numeric', 'constant.language', 'constant.language.boolean'], settings: { foreground: '#b9dcff' } },
		{ scope: ['keyword.operator', 'punctuation'], settings: { foreground: '#d1e2ef' } },
		{ scope: ['entity.name.type', 'entity.name.class', 'support.class', 'support.type'], settings: { foreground: '#ffffff' } },
		{ scope: ['support.function.builtin', 'variable.language', 'entity.name.command'], settings: { foreground: '#badff9' } },
	],
};

export default defineConfig({
	site: 'https://useshards.com',
	output: 'static',
	trailingSlash: 'ignore',
	integrations: [
		starlight({
			title: 'shard',
			description: 'The shard docs: install shard, then run isolated sandboxes for AI agents on a Linux server or a Mac that you own.',
			favicon: '/shard-mark.svg',
			head: [
				{ tag: 'link', attrs: { rel: 'icon', href: '/favicon.ico', sizes: '32x32' } },
				{ tag: 'link', attrs: { rel: 'apple-touch-icon', href: '/apple-touch-icon.png' } },
			],
			routeMiddleware: './src/routeData.ts',
			customCss: [
				'@fontsource-variable/geist/wght.css',
				'@fontsource-variable/jetbrains-mono/wght.css',
				'@fontsource/gugi/latin-400.css',
				'./src/styles/docs.css',
			],
			components: {
				SiteTitle: './src/components/starlight/SiteTitle.astro',
				SocialIcons: './src/components/starlight/SocialIcons.astro',
				ThemeSelect: './src/components/starlight/ThemeSelect.astro',
			},
			sidebar,
			// The landing's terminal panel, dark in both themes.
			expressiveCode: {
				themes: [terminal],
				useStarlightDarkModeSwitch: false,
				useStarlightUiThemeColors: false,
				styleOverrides: {
					borderRadius: '11px',
					borderColor: '#416a86',
					codeBackground: '#030b13',
					codeFontFamily: "'JetBrains Mono Variable', ui-monospace, monospace",
					uiFontFamily: "'JetBrains Mono Variable', ui-monospace, monospace",
					uiFontSize: '0.75rem',
					frames: {
						editorBackground: '#030b13',
						editorTabBarBackground: '#030b13',
						editorTabBarBorderBottomColor: '#20384b',
						editorActiveTabBackground: '#030b13',
						editorActiveTabForeground: '#e2f4ff',
						editorActiveTabIndicatorTopColor: 'transparent',
						editorActiveTabIndicatorBottomColor: '#91C4ED',
						editorActiveTabIndicatorHeight: '2px',
						terminalBackground: '#030b13',
						terminalTitlebarBackground: '#0c1c29',
						terminalTitlebarForeground: '#a9c7dc',
						terminalTitlebarBorderBottomColor: 'transparent',
						terminalTitlebarDotsOpacity: '0',
						frameBoxShadowCssValue: 'none',
						inlineButtonForeground: '#91C4ED',
						inlineButtonBorder: 'transparent',
					},
				},
			},
		}),
		react(),
		// The landing is a static file, so the sitemap learns of it here.
		sitemap({ customPages: ['https://useshards.com/'] }),
	],
	vite: {
		plugins: [tailwindcss()],
	},
});
