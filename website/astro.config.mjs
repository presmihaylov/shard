// @ts-check
import react from '@astrojs/react';
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

export default defineConfig({
	site: 'https://useshards.com',
	output: 'static',
	trailingSlash: 'ignore',
	integrations: [
		starlight({
			title: 'shard',
			description: 'Placeholder description for the shard documentation.',
			customCss: [
					'@fontsource-variable/geist/wght.css',
					'@fontsource-variable/outfit/wght.css',
					'@fontsource-variable/jetbrains-mono/wght.css',
					'@fontsource/dela-gothic-one/latin-400.css',
					'./src/styles/docs.css',
				],
			components: {
				SiteTitle: './src/components/starlight/SiteTitle.astro',
				SocialIcons: './src/components/starlight/SocialIcons.astro',
				ThemeSelect: './src/components/starlight/ThemeSelect.astro',
			},
			sidebar,
			// One dark code panel in both themes, as the nairi docs do.
			expressiveCode: {
				themes: ['github-dark'],
				useStarlightDarkModeSwitch: false,
				useStarlightUiThemeColors: false,
				styleOverrides: {
					borderRadius: '8px',
					borderColor: 'color-mix(in oklab, #ded6c9 14%, transparent)',
					codeBackground: '#322b26',
					codeFontFamily: "'JetBrains Mono Variable', ui-monospace, monospace",
					uiFontFamily: "'Geist Variable', ui-sans-serif, system-ui, sans-serif",
					frames: {
						editorBackground: '#322b26',
						editorTabBarBackground: '#322b26',
						editorActiveTabBackground: '#322b26',
						editorActiveTabIndicatorBottomColor: 'transparent',
						editorTabBarBorderBottomColor: 'color-mix(in oklab, #ded6c9 10%, transparent)',
						terminalBackground: '#322b26',
						terminalTitlebarBackground: '#322b26',
						terminalTitlebarBorderBottomColor: 'color-mix(in oklab, #ded6c9 10%, transparent)',
						terminalTitlebarForeground: 'color-mix(in oklab, #ded6c9 72%, transparent)',
						frameBoxShadowCssValue: 'none',
						inlineButtonForeground: 'color-mix(in oklab, #ded6c9 72%, transparent)',
						inlineButtonBorder: 'transparent',
					},
				},
			},
		}),
		react(),
	],
	vite: {
		plugins: [tailwindcss()],
	},
});
