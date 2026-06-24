# Vanity import hosting for `go.neatlogs.com`

This directory holds the static page that makes `go get go.neatlogs.com/...`
work. The Go module path is `go.neatlogs.com`; the code is hosted on GitHub at
`github.com/neatlogs/neatlogs-go`. The page below is the indirection that tells
the Go toolchain where the real repo is.

## How `go get go.neatlogs.com` resolves

1. `go get go.neatlogs.com/contrib/adk` makes an HTTPS request to
   `https://go.neatlogs.com/contrib/adk?go-get=1`.
2. The response must contain:
   `<meta name="go-import" content="go.neatlogs.com git https://github.com/neatlogs/neatlogs-go">`
3. Go then clones the module from GitHub. One meta tag covers the root module and
   all submodules (e.g. `go.neatlogs.com/contrib/adk`).

## Requirements

- The repo `github.com/neatlogs/neatlogs-go` must be **public** (so the Go module
  proxy and external users can fetch it).
- `https://go.neatlogs.com` and every subpath must return [`index.html`](index.html)
  (the meta tags are path-independent — serve the same file for all paths).

## Hosting options (pick one)

### A. GitHub Pages (zero-maintenance)
1. Push `index.html` to a Pages-served branch/repo.
2. Add a `CNAME` file containing `go.neatlogs.com`.
3. Point a DNS `CNAME` record `go.neatlogs.com → <org>.github.io`.
4. Configure the Pages site to serve `index.html` as the 404/catch-all so every
   subpath returns it (Pages serves `404.html` for unknown paths — copy
   `index.html` to `404.html`).

### B. Any static host / reverse proxy
Serve `index.html` for `go.neatlogs.com` and rewrite all subpaths to it, e.g.
nginx:

```nginx
server {
  server_name go.neatlogs.com;
  location / { try_files /index.html =404; }
}
```

### C. govanityurls (Google's tool)
Run [govanityurls](https://github.com/GoogleCloudPlatform/govanityurls) with a
`vanity.yaml`:

```yaml
host: go.neatlogs.com
paths:
  /:
    repo: https://github.com/neatlogs/neatlogs-go
    vcs: git
```

## Verify

```bash
curl -s "https://go.neatlogs.com/contrib/adk?go-get=1" | grep go-import
# should print the go-import meta tag

GOPROXY=direct go get go.neatlogs.com@latest
```
