# Pages

Atelier can publish a static website from a repository, like GitHub Pages.

## Turning it on

Sites are served from their **own host name**, never from Atelier's. A site's
HTML and JavaScript are written by whoever pushes to the repository; on the
app's origin they could read your session. Atelier recognises Pages requests
by their `Host` header and, on that host, serves sites and nothing else: no
app pages, no git, no sign-in, no cookies.

1. **DNS.** Add a name that points at the same server as Atelier. With
   AdGuard Home or Pi-hole, add a DNS rewrite, for example
   `pages.infinityhomelab.lan → <server IP>` (the same IP as
   `atelier.infinityhomelab.lan`).
2. **Proxy.** In Nginx Proxy Manager, add a proxy host:
   - Domain names: `pages.infinityhomelab.lan`
   - Scheme `http`, forward hostname/IP: the server IP (or `atelier` on a
     shared Docker network), forward port: the same port as the app
     (`3010` in `deploy/portainer/stack.yml`, `3000` inside the network).
   - Leave "Block common exploits" on; websockets aren't needed. Don't add
     any custom location or header that rewrites `Host`.
   - Optional: an SSL certificate for the name, then use `https://` below.
3. **Atelier.** Set `ATELIER_PAGES_URL=http://pages.infinityhomelab.lan` (or
   `https://…`) on the container and redeploy the stack.

`ATELIER_PAGES_URL` must be a different host from `ATELIER_PUBLIC_URL`;
if they match, Pages stays off and the log says why. Only its scheme and host
are used.

Other settings:

| Variable | Default | |
|---|---|---|
| `ATELIER_PAGES_MAX_MB` | `1024` | largest deploy, in MB |
| `ATELIER_PAGES_MAX_FILES` | `50000` | most files in a deploy |
| `ATELIER_PAGES_KEEP` | `5` | deploys kept for rolling back |

## Addresses

- `<pages>/<owner>/<repo>/` for any repository.
- The repository named **`<owner>.pages`** (for example `zoe.pages`) is the
  owner's own site, at `<pages>/<owner>/`. A path there that matches another
  published repository (`<pages>/zoe/blog/`) goes to that repository.

## Publishing

Open the repository's **Settings → Pages** (`/<owner>/<repo>/settings/pages`).

**A folder on a branch.** Pick the branch and a folder (`/` or e.g. `/docs`).
The files are published as they are, right away and on every push to that
branch. "Deploy now" republishes without a push.

**An Actions workflow.** The workflow builds the site and uploads it with the
built-in `atelier/deploy-pages` action. It needs no lockfile entry. Example
`.atelier/workflows/pages.yml`:

```yaml
name: Pages
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: [linux]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 22
      - run: npm ci && npm run build
      - uses: atelier/deploy-pages@v1
        with:
          path: dist          # the built site; default _site
```

The action packs the folder (regular files only; symlinks are skipped) and
posts it to `POST /api/pages/deploy` with the job token. Its output
`page_url` is the live address. The server accepts it only when:

- the token is a running Actions job's token, for **its own repository**
  (not another repository it can read, and never an agentic job's);
- the repository's Pages source is "An Actions workflow";
- the run came from a **push to the configured branch** (pull requests and
  other branches are refused).

Without the built-in action, any job can do the same with curl:

```sh
tar -czf site.tgz -C dist .
curl --fail -X POST -H "Authorization: Bearer $ATELIER_TOKEN" \
  --data-binary @site.tgz "$ATELIER_SERVER_URL/api/pages/deploy?repo=$GITHUB_REPOSITORY"
```

(`ATELIER_TOKEN` comes from `${{ secrets.ATELIER_TOKEN }}`.)

## Deploys and rollback

Each deploy is an immutable snapshot under `<data>/pages/sites/<repo id>/`.
A new deploy is written to a temporary folder and only becomes live once it
is complete, with one database update, so visitors never see half a site.
The last 5 good deploys (and the live one) are kept; the deploy list in
settings has **Roll back to this deploy** on each. A failed deploy (too big,
missing folder) is listed with its reason and never replaces the live site.
**Unpublish** takes the site offline and deletes all deploys.

## How sites are served

- `index.html` for folders (`/guide` redirects to `/guide/`), and
  `/about` finds `about.html`. No directory listings.
- Missing files get the site's own `404.html` with status 404, if it has one.
- Content types from the file extension; `X-Content-Type-Options: nosniff`.
- `ETag` and `Last-Modified`, so browsers revalidate cheaply.
- A sandboxing Content-Security-Policy: scripts run, but each page is in an
  opaque origin, so one site can't read another's pages or cookies on the
  shared Pages host. Resources may come from the site or any `https:` URL.
  Because of the sandbox, a site's own `fetch()` calls are cross-origin;
  Pages sends `Access-Control-Allow-Origin: *` so they still work.
- Paths with `..`, backslashes or NUL are refused; files are opened through
  a root confined to the snapshot.

## Private repositories

Private repositories **can't publish**. A private site would need a sign-in
on the Pages host, and every site shares that host: any published site's
script could then read a private site with the visitor's credentials. Rather
than build that badly, Pages refuses private repositories, and a site goes
offline the moment its repository is made private.

## Not done yet

- Custom domains.
- Pull request previews.
