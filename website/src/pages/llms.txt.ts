import type { APIRoute } from 'astro';
import { REPO_URL } from '../site';
import { groupedDocs, INSTALL, LANDING, markdownUrl, SITE } from '../seo';

export const GET: APIRoute = async () => {
	const groups = (await groupedDocs()).map(
		({ label, pages }) =>
			`## ${label}\n\n${pages.map((entry) => `- [${entry.data.title}](${markdownUrl(entry.id)}): ${entry.data.description ?? ''}`).join('\n')}`,
	);
	const text = [
		'# shard',
		"> shard runs isolated sandboxes for AI agents on one Linux server or Apple silicon Mac that you own. One binary is the CLI and the daemon, and it runs each sandbox on gVisor, Firecracker, Sysbox, runc or Apple's Virtualization framework.",
		`Install it with \`${INSTALL}\`. Every docs link below is the Markdown copy of a page; drop the \`.md\` for the HTML page. ${SITE}/llms-full.txt holds every docs page in one file.`,
		`## Site\n\n- [Homepage](${SITE}/): ${LANDING.description}\n- [Documentation](${SITE}/docs/): install, concepts, security, guides, SDKs and reference\n- [Install script](${SITE}/install): the shell script the install command runs\n- [Source code](${REPO_URL}): the repository, under the Apache-2.0 license`,
		...groups,
	];
	return new Response(`${text.join('\n\n')}\n`, { headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
};
