# image-patcher Docs Site

Documentation site for image-patch-operator, built with [Astro Starlight](https://starlight.astro.build). Supports English and Simplified Chinese.

## Local Development

```sh
cd docs/website
npm install
npm run dev
```

Opens at `http://localhost:4321`.

## Build

```sh
npm run build
```

Output goes to `docs/website/dist/`.

## Preview Build

```sh
npm run preview
```

## Deployment

### Vercel

1. Import the repository in [Vercel](https://vercel.com).
2. Set **Root Directory** to `docs/website`.
3. Framework preset: **Astro** (auto-detected).
4. Deploy — no environment variables required.

### Netlify

1. Connect the repository in [Netlify](https://www.netlify.com).
2. Set **Base directory** to `docs/website`.
3. **Build command:** `npm run build`
4. **Publish directory:** `docs/website/dist`

### GitHub Pages

Add a workflow at `.github/workflows/docs.yml`:

```yaml
name: Deploy Docs
on:
  push:
    branches: [main]
    paths:
      - 'docs/website/**'

jobs:
  deploy:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pages: write
      id-token: write
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 20
      - run: npm ci
        working-directory: docs/website
      - run: npm run build
        working-directory: docs/website
      - uses: actions/upload-pages-artifact@v3
        with:
          path: docs/website/dist
      - uses: actions/deploy-pages@v4
```

Enable **GitHub Pages** in repository settings and set source to **GitHub Actions**.

### Self-Hosted / Static

Copy the contents of `docs/website/dist/` to any static file server (nginx, S3 + CloudFront, etc.).

If the site is served from a subpath (e.g. `https://example.com/image-patcher/`), set `base` in `astro.config.mjs`:

```js
export default defineConfig({
  base: '/image-patcher/',
  integrations: [starlight({ ... })],
});
```
