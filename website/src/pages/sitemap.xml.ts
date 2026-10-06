import type { APIRoute } from 'astro';
import { docsPages, pageUrl, SITE } from '../seo';

export const GET: APIRoute = async () => {
	const urls = [`${SITE}/`, ...(await docsPages()).map((entry) => pageUrl(entry.id)).sort()];
	const body = urls.map((url) => `<url><loc>${url}</loc></url>`).join('');
	return new Response(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">${body}</urlset>\n`, {
		headers: { 'Content-Type': 'application/xml; charset=utf-8' },
	});
};
