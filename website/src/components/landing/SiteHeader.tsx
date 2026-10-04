import { useEffect, useState } from 'react';
import { NAV_LINKS } from '../../site';
import { icons } from '../icons';
import ThemeToggle from '../ThemeToggle';

const navLink =
	'px-3 py-1.5 text-sm font-medium rounded-md text-muted-foreground transition-all hover:text-foreground nairi-focus-ring';

export default function SiteHeader() {
	const [scrolled, setScrolled] = useState(false);
	const [open, setOpen] = useState(false);

	useEffect(() => {
		const onScroll = () => setScrolled(window.scrollY > 40);
		onScroll();
		window.addEventListener('scroll', onScroll, { passive: true });
		return () => window.removeEventListener('scroll', onScroll);
	}, []);

	useEffect(() => {
		if (!open) return;
		const onKey = (e: KeyboardEvent) => {
			if (e.key === 'Escape') setOpen(false);
		};
		window.addEventListener('keydown', onKey);
		return () => window.removeEventListener('keydown', onKey);
	}, [open]);

	return (
		<header
			className={`fixed inset-x-0 top-0 z-50 backdrop-blur-glass transition-colors duration-300 ${scrolled || open ? 'bg-background/90' : 'bg-background/40'}`}
		>
			<div className="mx-auto flex max-w-7xl items-center gap-6 px-6 py-3 md:px-8">
				<a href="/" className="font-brand text-lg leading-none text-foreground nairi-focus-ring">
					shard
				</a>
				<nav aria-label="Main" className="hidden items-center gap-1 md:flex">
					{NAV_LINKS.map((link) => (
						<a key={link.href} href={link.href} className={navLink}>
							{link.label}
						</a>
					))}
				</nav>
				<div className="ml-auto flex items-center gap-2">
					<ThemeToggle />
					<a href="/docs/" className="vbtn vbtn--sm vbtn--neutral vbtn--primary nairi-focus-ring hidden md:inline-flex">
						<span>Read the docs</span>
					</a>
					<button
						type="button"
						aria-label={open ? 'Close menu' : 'Open menu'}
						aria-expanded={open}
						aria-controls="mobile-menu"
						onClick={() => setOpen(!open)}
						className="flex size-9 items-center justify-center rounded-md text-muted-foreground transition-all hover:bg-foreground/[0.05] hover:text-foreground nairi-focus-ring md:hidden"
					>
						<svg
							viewBox="0 0 24 24"
							width={20}
							height={20}
							aria-hidden="true"
							className="nairi-icon"
							dangerouslySetInnerHTML={{ __html: open ? icons.close : icons.hamburgerMenu }}
						/>
					</button>
				</div>
			</div>
			{open && (
				<nav
					id="mobile-menu"
					aria-label="Mobile"
					className="mx-auto max-w-7xl space-y-0.5 border-t border-foreground/[0.07] px-4 py-3 md:hidden"
				>
					{NAV_LINKS.map((link) => (
						<a
							key={link.href}
							href={link.href}
							onClick={() => setOpen(false)}
							className="block rounded-md px-2.5 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-foreground/[0.05] hover:text-foreground"
						>
							{link.label}
						</a>
					))}
				</nav>
			)}
		</header>
	);
}
