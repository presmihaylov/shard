import { OGImageRoute } from 'astro-og-canvas';
import { docsPages, LANDING, pageTitles } from '../../seo';

const titles = await pageTitles();
const pages = Object.fromEntries([
	['index', LANDING],
	...(await docsPages()).map((entry) => [entry.id, { title: titles.get(entry.id) ?? entry.data.title, description: entry.data.description ?? '' }]),
]);

export const { getStaticPaths, GET } = await OGImageRoute({
	pages,
	getImageOptions: (_, page: typeof LANDING) => ({
		title: page.title,
		description: page.description,
		logo: { path: './public/logo-simple.png', size: [120] },
		bgGradient: [
			[3, 11, 19],
			[5, 41, 85],
		],
		border: { color: [45, 129, 190], width: 12, side: 'inline-start' },
		padding: 72,
		font: {
			title: { color: [228, 243, 252], size: 76, lineHeight: 1.15, families: ['Gugi'] },
			description: { color: [154, 175, 192], size: 36, lineHeight: 1.4, families: ['Geist'] },
		},
		fonts: [
			'./node_modules/@fontsource/gugi/files/gugi-latin-400-normal.woff2',
			'./node_modules/@fontsource-variable/geist/files/geist-latin-wght-normal.woff2',
		],
	}),
});
