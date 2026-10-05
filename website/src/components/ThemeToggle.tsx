import { useEffect, useState } from 'react';

type Theme = 'light' | 'dark';

// Starlight's own key, so its pre-paint script restores the choice on every page.
const storageKey = 'starlight-theme';

function currentTheme(): Theme {
	return document.documentElement.dataset.theme === 'light' ? 'light' : 'dark';
}

export default function ThemeToggle() {
	const [theme, setTheme] = useState<Theme | null>(null);

	useEffect(() => setTheme(currentTheme()), []);

	const toggle = () => {
		const next: Theme = currentTheme() === 'dark' ? 'light' : 'dark';
		document.documentElement.dataset.theme = next;
		localStorage.setItem(storageKey, next);
		setTheme(next);
	};

	return (
		<button
			type="button"
			onClick={toggle}
			aria-label="Toggle light and dark theme"
			data-theme-toggle=""
			className="theme-toggle"
		>
			<svg
				viewBox="0 0 24 24"
				width={16}
				height={16}
				fill="none"
				stroke="currentColor"
				strokeWidth={1.8}
				strokeLinecap="round"
				strokeLinejoin="round"
				aria-hidden="true"
			>
				{theme === 'dark' ? (
					<>
						<circle cx="12" cy="12" r="4" />
						<path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
					</>
				) : (
					<path d="M20.5 14.5A8.5 8.5 0 0 1 9.5 3.5a8.5 8.5 0 1 0 11 11Z" />
				)}
			</svg>
		</button>
	);
}
