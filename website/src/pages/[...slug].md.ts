import type { APIRoute, GetStaticPaths } from 'astro';
import { type DocsEntry, docsPages, toMarkdown } from '../seo';

export const getStaticPaths = (async () => (await docsPages()).map((entry) => ({ params: { slug: entry.id }, props: { entry } }))) satisfies GetStaticPaths;

export const GET: APIRoute<{ entry: DocsEntry }> = ({ props }) =>
	new Response(toMarkdown(props.entry), { headers: { 'Content-Type': 'text/markdown; charset=utf-8' } });
