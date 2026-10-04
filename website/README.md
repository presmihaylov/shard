# website

The useshards.com site: an Astro + Starlight static build. The landing page is
at `/` and the docs are at `/docs`. It lives outside the Go module and outside
`make check`, and it ships independently of the binary releases.

## Local dev

Node 22 or newer.

```
npm ci
npm run dev       # http://localhost:4321, live reload
npm run check     # astro check: types and content
npm run build     # static output in dist/, Pagefind index included
npm run verify    # every route exists and every internal link in dist/ resolves
npm run preview   # serve dist/ as Vercel would
```

`.github/workflows/website.yml` runs `check`, `build` and `verify` on every pull
request that touches `website/`.

## Layout

```
src/pages/index.astro        the landing page
src/layouts/Landing.astro    the landing shell: header, main, footer
src/components/landing/      landing header, footer and card
src/components/starlight/    the Starlight overrides: site title, social icons, theme toggle
src/content/docs/docs/       the docs pages, served under /docs
src/styles/tokens.css        the design tokens: every color, font, radius and shadow
src/styles/ui/               button, badge, banner and surface styles
src/styles/docs.css          the Starlight skin, mapped onto the tokens
src/styles/landing.css       the landing entry: Tailwind plus the tokens
```

A color or a font changes in `tokens.css` only. The landing page and the docs
share the theme through the `starlight-theme` key in `localStorage`.

## Vercel

One project serves the whole site. Connect it in the Vercel dashboard:

1. Import `presmihaylov/shard`.
2. Set **Root Directory** to `website`. Leave the framework preset on Astro.
3. Add the `useshards.com` domain to the project.

`vercel.json` pins the install, build and output settings. Its `ignoreCommand`
skips a deploy when a commit changes nothing under `website/`.
