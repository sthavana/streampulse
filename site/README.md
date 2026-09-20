# The landing page

A single static HTML file. No framework, no build step, no npm, and nothing
fetched from a CDN at runtime — which is the only honest way to advertise a
project whose pitch is that it has no dependencies.

```
site/
  index.html     the whole page, styles inline
  img/hero.png   the multiviewer wall, first tile row
  img/operator.png  the operator page, cropped to a card boundary
```

Both images are crops of the screenshots in `docs/`. Regenerate them with
`make site-images` after retaking either one.

## Deploying

`vercel.json` in the repository root points Vercel at this directory with no
build step. Either:

- **Connect the repo** at [vercel.com/new](https://vercel.com/new) and pick this
  repository. Vercel reads `vercel.json`, serves `site/`, and redeploys on every
  push to `main`. This is the one to use.
- **Or from the command line**, once: `vercel login`, then `vercel --prod` from
  the repository root.

There is no backend and nothing to keep running. The prober cannot be deployed
here and is not meant to be: it is a long-lived process holding cross-poll
state in memory, which is the opposite of what a serverless platform provides.

## Checking it before deploying

```
open site/index.html
```

It is a file:// page with no scripts, so what you see locally is what ships.
