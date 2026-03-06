# Joulie Docs Website (Hugo + Docsy)

This directory contains the documentation website.

## Content structure

- `content/en/_index.md`: homepage
- `content/en/docs/`: documentation pages
- `content/en/docs/getting-started/`: install and runtime setup
- `content/en/docs/architecture/`: policy, operator, telemetry, metrics
- `content/en/docs/simulator/`: simulator architecture, algorithms, pod compatibility
- `content/en/docs/experiments/`: experiment documentation
- `static/images/logo.png`: website logo

## Prerequisites

- Go (module support enabled)
- Hugo **extended**

Check packages versions:

```bash
go version
hugo version
```

## First-time setup

From repo root:

```bash
cd website
hugo mod get github.com/google/docsy@latest
hugo mod get github.com/google/docsy/dependencies@latest
```

## Run locally (live preview)

```bash
cd website
hugo server --disableFastRender
```

Open:

- `http://localhost:1313/Joulie/`

## Build locally

```bash
cd website
hugo --gc --minify --baseURL http://localhost:1313/Joulie/
```

Output is generated in:

- `website/public/`

## Build like GitHub Pages

```bash
cd website
hugo --gc --minify --baseURL https://joulie-k8s.github.io/Joulie/
```

## GitHub Pages layout

This repository publishes docs to the `gh-pages` branch with multiple sub-sites:

- `https://joulie-k8s.github.io/Joulie/main/` (main branch docs)
- `https://joulie-k8s.github.io/Joulie/stable/` (latest release docs)
- `https://joulie-k8s.github.io/Joulie/versions/<tag>/` (versioned release docs)
- `https://joulie-k8s.github.io/Joulie/previews/pr-<n>/` (PR previews)

Workflows:

- `.github/workflows/docs-main.yml`
- `.github/workflows/docs-release.yml`
- `.github/workflows/docs-preview.yml`

## Troubleshooting

If modules are missing or stale:

```bash
cd website
hugo mod clean
hugo mod get github.com/google/docsy@latest
hugo mod get github.com/google/docsy/dependencies@latest
```

If the page looks empty:

```bash
cd website
rm -rf public resources
hugo server --disableFastRender
```

Then open exactly:

- `http://localhost:1313/Joulie/`
