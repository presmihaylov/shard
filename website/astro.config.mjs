// @ts-check
import react from '@astrojs/react';
import starlight from '@astrojs/starlight';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'astro/config';

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
			sidebar: [
				{
					label: 'Start here',
					items: [
						{ label: 'Overview', slug: 'docs' },
						{ label: 'Components', slug: 'docs/components' },
					],
				},
				{
					label: 'Guides',
					items: [{ label: 'Placeholder guide', slug: 'docs/guides/placeholder' }],
				},
			],
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
