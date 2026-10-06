import { defineRouteMiddleware } from '@astrojs/starlight/route-data';
import { breadcrumbs, jsonLd, ogImageUrl, organization, pageTitles, pageUrl, SITE_NAME, WEBSITE_ID } from './seo';

const titles = pageTitles();

export const onRequest = defineRouteMiddleware(async (context) => {
	const route = context.locals.starlightRoute;
	const meta = (key: 'name' | 'property', name: string, content: string) => ({ tag: 'meta' as const, attrs: { [key]: name, content } });

	// The 404 page answers at every missing path, so it names no URL of its own.
	if (route.id === '404') {
		route.head = route.head.filter((tag) => tag.attrs?.rel !== 'canonical' && tag.attrs?.property !== 'og:url');
		route.head.push(meta('name', 'robots', 'noindex, follow'));
		return;
	}

	const all = await titles;
	const title = all.get(route.id) ?? route.entry.data.title;
	const description = route.entry.data.description ?? '';
	const url = pageUrl(route.id);
	const image = ogImageUrl(route.id);
	const alt = `${title}, shard docs`;

	for (const tag of route.head) {
		if (tag.tag === 'title') tag.content = `${title} | ${SITE_NAME}`;
		if (tag.attrs?.property === 'og:title') tag.attrs.content = title;
		if (tag.attrs?.property === 'og:locale') tag.attrs.content = 'en_US';
	}

	const crumbs = breadcrumbs(route.id, route.entry.data.title, new Set(all.keys()));
	route.head.push(
		meta('name', 'robots', 'index, follow'),
		meta('property', 'og:image', image),
		meta('property', 'og:image:width', '1200'),
		meta('property', 'og:image:height', '630'),
		meta('property', 'og:image:type', 'image/png'),
		meta('property', 'og:image:alt', alt),
		meta('name', 'twitter:title', title),
		meta('name', 'twitter:description', description),
		meta('name', 'twitter:image', image),
		meta('name', 'twitter:image:alt', alt),
		{
			tag: 'script',
			attrs: { type: 'application/ld+json' },
			content: jsonLd([
				{
					'@type': 'TechArticle',
					'@id': `${url}#article`,
					headline: title,
					description,
					url,
					mainEntityOfPage: url,
					image,
					inLanguage: 'en',
					isPartOf: { '@type': 'WebSite', '@id': WEBSITE_ID, name: SITE_NAME },
					author: { '@type': 'Organization', '@id': organization['@id'], name: SITE_NAME, url: organization.url },
					publisher: organization,
				},
				{
					'@type': 'BreadcrumbList',
					itemListElement: crumbs.map((crumb, index) => ({ '@type': 'ListItem', position: index + 1, name: crumb.name, item: crumb.url })),
				},
			]),
		},
	);
});
