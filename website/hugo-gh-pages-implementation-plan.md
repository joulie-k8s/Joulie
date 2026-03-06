# Hugo + GitHub Pages multi-site docs implementation plan

## Goal

Implement a GitHub Pages deployment model for the `Joulie` repo that supports all of the following at the same time:

- `stable`: docs for the latest published release
- `main`: docs built from the `main` branch
- `versions`: docs for tagged releases, selectable from a version dropdown
- `PR preview`: preview docs for pull requests

This plan assumes the repo is a **project site**, so the public base path is:

```text
https://joulie-k8s.github.io/Joulie/
```

That means every Hugo build must use a `baseURL` that includes `/Joulie/...`.

---

## Decision

Use **GitHub Pages publishing from the `gh-pages` branch**.

### Why

You do not want a single deployed site artifact. You want one published tree containing several independently updated sub-sites:

- `main/`
- `stable/`
- `versions/<tag>/`
- `previews/pr-<n>/`

That is much easier to manage by treating `gh-pages` as the published filesystem for GitHub Pages.

---

## Required GitHub Pages settings

In the repository:

`Settings -> Pages`

Set:

```text
Source: Deploy from a branch
Branch: gh-pages
Folder: /(root)
```

Do **not** set Pages back to `GitHub Actions` if the workflows are publishing built files into `gh-pages`.

---

## Where the layout lives

The following layout is in the **`gh-pages` branch**.

It is **not**:

- the `main` branch source tree
- the temporary GitHub Actions workspace

```text
gh-pages/
├── .nojekyll
├── index.html
├── 404.html
├── versions.json
├── stable/
├── main/
├── versions/
│   ├── v0.1.0/
│   ├── v0.2.0/
│   └── ...
└── previews/
    ├── pr-12/
    ├── pr-18/
    └── ...
```

Public URLs:

```text
https://joulie-k8s.github.io/Joulie/                       # root redirect
https://joulie-k8s.github.io/Joulie/main/
https://joulie-k8s.github.io/Joulie/stable/
https://joulie-k8s.github.io/Joulie/versions/v0.2.0/
https://joulie-k8s.github.io/Joulie/previews/pr-123/
```

---

## Ownership model

Each workflow owns a specific part of `gh-pages`:

- `docs-main.yml` owns `main/`
- `docs-release.yml` owns `stable/` and `versions/<tag>/`
- `docs-preview.yml` owns `previews/pr-<number>/`

Shared root files:

- `index.html`
- `404.html`
- `versions.json`
- `.nojekyll`

Those root files should be managed only by `docs-main.yml` and `docs-release.yml`.

---

## Concurrency model

All workflows that write to `gh-pages` must use the **same concurrency group**, otherwise concurrent writes can race and overwrite each other.

Use:

```yaml
concurrency:
  group: gh-pages-publish
  cancel-in-progress: false
```

This serializes writes to `gh-pages` across:

- main docs builds
- release docs builds
- preview docs builds
- preview cleanup

---

## Version manifest contract

A root-level `versions.json` file is used by the docs UI to populate the version dropdown.

Recommended format:

```json
{
  "default": "stable",
  "versions": [
    { "name": "stable", "path": "/Joulie/stable/" },
    { "name": "main", "path": "/Joulie/main/" },
    { "name": "v0.2.0", "path": "/Joulie/versions/v0.2.0/" },
    { "name": "v0.1.0", "path": "/Joulie/versions/v0.1.0/" }
  ]
}
```

Notes:

- Before the first release, `default` can be `main`.
- After the first release, `default` should become `stable`.
- The release workflow should prepend the newest version entry.
- `stable` is a moving alias to the latest published release.

---

## Implementation steps

### 1. Fix GitHub Pages settings

Change the repo Pages configuration to:

```text
Deploy from a branch -> gh-pages -> /(root)
```

### 2. Split the workflows by responsibility

Create three workflow files:

- `.github/workflows/docs-main.yml`
- `.github/workflows/docs-release.yml`
- `.github/workflows/docs-preview.yml`

### 3. Publish main docs under `/main/`

On pushes to `main`, build the Hugo site with base URL:

```text
https://joulie-k8s.github.io/Joulie/main/
```

Publish to:

```text
gh-pages:/main/
```

### 4. Publish releases twice

When a GitHub Release is published:

- build once for `versions/<tag>/`
- build a second time for `stable/`

This is necessary because Hugo bakes the `baseURL` into the generated output.

A build for `/versions/vX.Y.Z/` must not be reused for `/stable/`.

### 5. Publish PR previews under `/previews/pr-<number>/`

For pull requests from the same repository, build the docs with base URL:

```text
https://joulie-k8s.github.io/Joulie/previews/pr-<number>/
```

Publish to:

```text
gh-pages:/previews/pr-<number>/
```

On PR close, remove that directory from `gh-pages`.

### 6. Bootstrap the root

The first successful docs workflow should ensure that `gh-pages` contains:

- `.nojekyll`
- `index.html`
- `404.html`
- `versions.json`

Root redirect behavior:

- before first release: `/` redirects to `/main/`
- after first release: `/` redirects to `/stable/`

### 7. Add the version dropdown in Docsy

Separate from the workflows, add a small Docsy navbar partial that:

- fetches `/Joulie/versions.json`
- populates a `<select>`
- navigates to the selected version path

The first version of the dropdown can simply send the user to the selected version root.

---

## File-by-file plan for Codex

### A. `.github/workflows/docs-main.yml`

```yaml
name: Docs Main

on:
  push:
    branches: ["main"]
    paths:
      - "website/**"
      - ".github/workflows/docs-main.yml"
      - ".github/workflows/docs-release.yml"
      - ".github/workflows/docs-preview.yml"
      - "README.md"
  workflow_dispatch:

permissions:
  contents: write

concurrency:
  group: gh-pages-publish
  cancel-in-progress: false

jobs:
  deploy-main:
    runs-on: ubuntu-latest
    env:
      HUGO_VERSION: 0.157.0
      SITE_ROOT_PATH: /${{ github.event.repository.name }}
      MAIN_URL: https://${{ github.repository_owner }}.github.io/${{ github.event.repository.name }}/main/
    steps:
      - name: Checkout
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version-file: website/go.mod

      - name: Setup Hugo
        uses: peaceiris/actions-hugo@v3
        with:
          hugo-version: ${{ env.HUGO_VERSION }}
          extended: true

      - name: Setup Node
        uses: actions/setup-node@v4
        with:
          node-version: "20"

      - name: Install PostCSS toolchain
        working-directory: website
        run: |
          npm install --no-save postcss postcss-cli autoprefixer

      - name: Build main docs
        working-directory: website
        run: |
          hugo mod get github.com/google/docsy@latest
          hugo mod get github.com/google/docsy/dependencies@latest
          hugo --gc --minify --baseURL "${{ env.MAIN_URL }}"

      - name: Deploy main docs to gh-pages/main
        uses: peaceiris/actions-gh-pages@v4
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
          publish_branch: gh-pages
          publish_dir: website/public
          destination_dir: main
          keep_files: true
          force_orphan: false

      - name: Ensure bootstrap files and versions manifest
        run: |
          set -euo pipefail

          git clone --depth 1 --branch gh-pages \
            "https://x-access-token:${{ secrets.GITHUB_TOKEN }}@github.com/${{ github.repository }}.git" \
            gh-pages

          cd gh-pages

          touch .nojekyll

          if [ ! -f index.html ]; then
            cat > index.html <<EOF2
          <!doctype html>
          <html lang="en">
            <head>
              <meta charset="utf-8">
              <meta http-equiv="refresh" content="0; url=main/">
              <title>Redirecting…</title>
              <link rel="canonical" href="main/">
            </head>
            <body>
              <p>Redirecting to <a href="main/">main docs</a>.</p>
            </body>
          </html>
          EOF2
          fi

          if [ ! -f 404.html ]; then
            cat > 404.html <<EOF2
          <!doctype html>
          <html lang="en">
            <head>
              <meta charset="utf-8">
              <title>Page not found</title>
            </head>
            <body>
              <h1>Page not found</h1>
              <p>Try the <a href="main/">main docs</a>.</p>
            </body>
          </html>
          EOF2
          fi

          FILE="versions.json"
          ROOT="${{ env.SITE_ROOT_PATH }}"

          if [ ! -f "$FILE" ]; then
            printf '{"default":"main","versions":[{"name":"main","path":"%s/main/"}]}' "$ROOT" > "$FILE"
          else
            tmp="$(mktemp)"
            jq --arg main "$ROOT/main/" '
              .default = (.default // "main")
              | .versions = (
                  [{"name":"main","path":$main}]
                  + ((.versions // []) | map(select(.name != "main")))
                )
            ' "$FILE" > "$tmp"
            mv "$tmp" "$FILE"
          fi

          git config user.name "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git add .nojekyll index.html 404.html versions.json
          if ! git diff --cached --quiet; then
            git commit -m "docs: update main docs metadata"
            git push
          fi
```

---

### B. `.github/workflows/docs-release.yml`

```yaml
name: Docs Release

on:
  release:
    types: [published]
  workflow_dispatch:

permissions:
  contents: write

concurrency:
  group: gh-pages-publish
  cancel-in-progress: false

jobs:
  deploy-release:
    runs-on: ubuntu-latest
    env:
      HUGO_VERSION: 0.157.0
      VERSION: ${{ github.event.release.tag_name }}
      SITE_ROOT_PATH: /${{ github.event.repository.name }}
      VERSION_URL: https://${{ github.repository_owner }}.github.io/${{ github.event.repository.name }}/versions/${{ github.event.release.tag_name }}/
      STABLE_URL: https://${{ github.repository_owner }}.github.io/${{ github.event.repository.name }}/stable/
    steps:
      - name: Checkout release tag
        uses: actions/checkout@v4
        with:
          ref: refs/tags/${{ github.event.release.tag_name }}
          fetch-depth: 0

      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version-file: website/go.mod

      - name: Setup Hugo
        uses: peaceiris/actions-hugo@v3
        with:
          hugo-version: ${{ env.HUGO_VERSION }}
          extended: true

      - name: Setup Node
        uses: actions/setup-node@v4
        with:
          node-version: "20"

      - name: Install PostCSS toolchain
        working-directory: website
        run: |
          npm install --no-save postcss postcss-cli autoprefixer

      - name: Resolve Hugo modules
        working-directory: website
        run: |
          hugo mod get github.com/google/docsy@latest
          hugo mod get github.com/google/docsy/dependencies@latest

      - name: Build versioned docs
        working-directory: website
        run: |
          hugo --gc --minify \
            --baseURL "${{ env.VERSION_URL }}" \
            --destination ../public-version

      - name: Build stable docs
        working-directory: website
        run: |
          hugo --gc --minify \
            --baseURL "${{ env.STABLE_URL }}" \
            --destination ../public-stable

      - name: Deploy versioned docs to gh-pages/versions/${{ env.VERSION }}
        uses: peaceiris/actions-gh-pages@v4
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
          publish_branch: gh-pages
          publish_dir: public-version
          destination_dir: versions/${{ env.VERSION }}
          keep_files: true
          force_orphan: false

      - name: Deploy stable docs to gh-pages/stable
        uses: peaceiris/actions-gh-pages@v4
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
          publish_branch: gh-pages
          publish_dir: public-stable
          destination_dir: stable
          keep_files: true
          force_orphan: false

      - name: Update root redirect and versions manifest
        run: |
          set -euo pipefail

          git clone --depth 1 --branch gh-pages \
            "https://x-access-token:${{ secrets.GITHUB_TOKEN }}@github.com/${{ github.repository }}.git" \
            gh-pages

          cd gh-pages

          touch .nojekyll

          cat > index.html <<EOF2
          <!doctype html>
          <html lang="en">
            <head>
              <meta charset="utf-8">
              <meta http-equiv="refresh" content="0; url=stable/">
              <title>Redirecting…</title>
              <link rel="canonical" href="stable/">
            </head>
            <body>
              <p>Redirecting to <a href="stable/">stable docs</a>.</p>
            </body>
          </html>
          EOF2

          cat > 404.html <<EOF2
          <!doctype html>
          <html lang="en">
            <head>
              <meta charset="utf-8">
              <title>Page not found</title>
            </head>
            <body>
              <h1>Page not found</h1>
              <p>Try the <a href="stable/">stable docs</a> or the <a href="main/">main docs</a>.</p>
            </body>
          </html>
          EOF2

          FILE="versions.json"
          ROOT="${{ env.SITE_ROOT_PATH }}"
          VERSION_NAME="${{ env.VERSION }}"

          if [ ! -f "$FILE" ]; then
            printf '{"default":"stable","versions":[{"name":"stable","path":"%s/stable/"},{"name":"main","path":"%s/main/"},{"name":"%s","path":"%s/versions/%s/"}]}' \
              "$ROOT" "$ROOT" "$VERSION_NAME" "$ROOT" "$VERSION_NAME" > "$FILE"
          else
            tmp="$(mktemp)"
            jq \
              --arg stable "$ROOT/stable/" \
              --arg main "$ROOT/main/" \
              --arg version_name "$VERSION_NAME" \
              --arg version_path "$ROOT/versions/$VERSION_NAME/" '
              .default = "stable"
              | .versions = (
                  [{"name":"stable","path":$stable}]
                  + [{"name":"main","path":$main}]
                  + [{"name":$version_name,"path":$version_path}]
                  + ((.versions // []) | map(select(.name != "stable" and .name != "main" and .name != $version_name)))
                )
            ' "$FILE" > "$tmp"
            mv "$tmp" "$FILE"
          fi

          git config user.name "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git add .nojekyll index.html 404.html versions.json
          if ! git diff --cached --quiet; then
            git commit -m "docs: publish ${VERSION_NAME}"
            git push
          fi
```

---

### C. `.github/workflows/docs-preview.yml`

```yaml
name: Docs Preview

on:
  pull_request:
    types: [opened, synchronize, reopened, closed]
    paths:
      - "website/**"
      - ".github/workflows/docs-preview.yml"
      - ".github/workflows/docs-main.yml"
      - ".github/workflows/docs-release.yml"
      - "README.md"

permissions:
  contents: write
  pull-requests: write

concurrency:
  group: gh-pages-publish
  cancel-in-progress: false

jobs:
  preview:
    if: github.event.action != 'closed'
    runs-on: ubuntu-latest
    env:
      HUGO_VERSION: 0.157.0
      PREVIEW_DIR: previews/pr-${{ github.event.pull_request.number }}
      PREVIEW_URL: https://${{ github.repository_owner }}.github.io/${{ github.event.repository.name }}/previews/pr-${{ github.event.pull_request.number }}/
    steps:
      - name: Checkout
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version-file: website/go.mod

      - name: Setup Hugo
        uses: peaceiris/actions-hugo@v3
        with:
          hugo-version: ${{ env.HUGO_VERSION }}
          extended: true

      - name: Setup Node
        uses: actions/setup-node@v4
        with:
          node-version: "20"

      - name: Install PostCSS toolchain
        working-directory: website
        run: |
          npm install --no-save postcss postcss-cli autoprefixer

      - name: Build preview site
        working-directory: website
        run: |
          hugo mod get github.com/google/docsy@latest
          hugo mod get github.com/google/docsy/dependencies@latest
          hugo --gc --minify --baseURL "${{ env.PREVIEW_URL }}"

      - name: Deploy preview to gh-pages
        if: github.event.pull_request.head.repo.full_name == github.repository
        uses: peaceiris/actions-gh-pages@v4
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
          publish_branch: gh-pages
          publish_dir: website/public
          destination_dir: ${{ env.PREVIEW_DIR }}
          keep_files: true
          force_orphan: false

      - name: Comment preview URL
        if: github.event.pull_request.head.repo.full_name == github.repository
        uses: peter-evans/create-or-update-comment@v4
        with:
          issue-number: ${{ github.event.pull_request.number }}
          body-includes: "<!-- docs-preview-comment -->"
          body: |
            <!-- docs-preview-comment -->
            Docs preview is ready:

            `${{ env.PREVIEW_URL }}`
          edit-mode: replace

  cleanup:
    if: github.event.action == 'closed'
    runs-on: ubuntu-latest
    env:
      PREVIEW_DIR: previews/pr-${{ github.event.pull_request.number }}
    steps:
      - name: Checkout gh-pages
        uses: actions/checkout@v4
        with:
          ref: gh-pages
          fetch-depth: 0
        continue-on-error: true

      - name: Remove preview directory
        run: |
          if [ ! -d .git ]; then
            echo "gh-pages branch does not exist yet; nothing to clean."
            exit 0
          fi
          rm -rf "${{ env.PREVIEW_DIR }}"
          git config user.name "github-actions[bot]"
          git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
          git add -A
          if git diff --cached --quiet; then
            echo "No preview directory to remove."
            exit 0
          fi
          git commit -m "docs: remove preview for PR #${{ github.event.pull_request.number }}"
          git push
```

---

## Additional implementation notes for Codex

### 1. Preserve files in `gh-pages`

Every deployment step using `peaceiris/actions-gh-pages` must keep:

```yaml
keep_files: true
force_orphan: false
```

Otherwise one workflow may wipe out content owned by another workflow.

### 2. Preview deployments from forks

The preview workflow intentionally deploys only when:

```yaml
if: github.event.pull_request.head.repo.full_name == github.repository
```

This avoids permission issues and unsafe writes for forked PRs.

### 3. Release trigger source of truth

Use GitHub Releases as the source of truth for `stable` and `versions/<tag>`.

The workflow uses:

```yaml
on:
  release:
    types: [published]
```

That means:

- `stable` updates only when a release is published
- `versions/<tag>` is created only for released tags

### 4. Root redirect policy

Expected behavior:

- before first release, root redirects to `main/`
- after first release, root redirects to `stable/`

### 5. Version ordering

`docs-release.yml` prepends the newest release near the top of `versions.json`.

If needed later, semantic version sorting can be added.

### 6. Version dropdown UI is a separate task

The workflows alone do not implement the Docsy dropdown.

That should be a separate follow-up task:

- create a Docsy override partial in `website/layouts/partials/...`
- fetch `/Joulie/versions.json`
- render the dropdown
- navigate on selection

### 7. Optional future enhancement

Later, the release workflow could also generate a machine-readable alias file, for example:

```json
{
  "stable": "v0.2.0"
}
```

This is optional.

---

## Validation checklist

### Main docs

- push a docs change to `main`
- confirm workflow success
- confirm `gh-pages/main/` was updated
- open `https://joulie-k8s.github.io/Joulie/main/`

### Root redirect before release

- confirm `https://joulie-k8s.github.io/Joulie/` redirects to `/Joulie/main/`

### Release docs

- publish a GitHub Release, e.g. `v0.1.0`
- confirm workflow success
- confirm `gh-pages/stable/` exists
- confirm `gh-pages/versions/v0.1.0/` exists
- confirm root now redirects to `/Joulie/stable/`
- confirm `versions.json` contains `stable`, `main`, and `v0.1.0`

### PR preview

- open a PR from a branch in the same repo
- confirm preview workflow success
- confirm preview comment appears on the PR
- open `https://joulie-k8s.github.io/Joulie/previews/pr-<n>/`
- close the PR and confirm preview directory is removed

### No cross-workflow clobbering

- run main docs and preview workflows close together
- confirm both outputs still exist after both complete

---

## Suggested rollout order

1. merge `docs-main.yml`
2. verify `main/` works
3. merge `docs-release.yml`
4. publish first release and verify `stable/` + `versions/<tag>/`
5. merge `docs-preview.yml`
6. add the Docsy version dropdown

---

## References

Authoritative references used for this plan:

- GitHub Docs: Configuring a publishing source for your GitHub Pages site
  - https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site
- GitHub Docs: Using custom workflows with GitHub Pages
  - https://docs.github.com/en/pages/getting-started-with-github-pages/using-custom-workflows-with-github-pages
- Hugo Docs: Host on GitHub Pages
  - https://gohugo.io/host-and-deploy/host-on-github-pages/
