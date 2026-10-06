import type { APIRoute } from 'astro';
import { groupedDocs, toMarkdown } from '../seo';

export const GET: APIRoute = async () => {
	const pages = (await groupedDocs()).flatMap(({ pages }) => pages.map(toMarkdown));
	return new Response(pages.join('\n---\n\n'), { headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
};
