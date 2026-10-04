import { useEffect, useState } from 'react';
import { icons } from './icons';

type Theme = 'light' | 'dark';

// Starlight's own key, so the landing and the docs share one choice.
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
			className="inline-flex size-8 cursor-pointer items-center justify-center rounded-full text-foreground/75 outline-none transition-all hover:bg-muted hover:text-foreground nairi-focus-ring"
		>
			<svg
				viewBox="0 0 24 24"
				width={16}
				height={16}
				aria-hidden="true"
				className="nairi-icon"
				dangerouslySetInnerHTML={{ __html: theme === 'dark' ? icons.sun : icons.moon }}
			/>
		</button>
	);
}
