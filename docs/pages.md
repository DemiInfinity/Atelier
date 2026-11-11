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
- the run came from a **push to the configured branch** (other branches are
  refused), or from a **pull request**, in which case the upload becomes that
  pull request's [preview](#pull-request-previews), never the site.

To get previews from a workflow, add `pull_request` to its triggers:

```yaml
on:
  push:
    branches: [main]
  pull_request:
```

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

## Custom domains

A site can have one host name of its own, such as `docs.example.com`, set in
**Settings → Pages → Custom domain**. The site is then served at the root of
that name (`https://docs.example.com/`, not under `/<owner>/<repo>/`); its
Pages address keeps working too.

### Verification

Nothing is served on a name until it's verified. Adding the name gives the
site a random token; prove the name is yours with either:

- a **TXT record** named `_atelier-pages.docs.example.com` whose value is the
  token (recommended; keep it in place), or
- a **file** at `https://docs.example.com/.well-known/atelier-pages/<token>`
  containing the token, served by whatever answers for the name today. If
  that's this site, commit `.well-known/atelier-pages/<token>` into it.
  Atelier fetches it over HTTPS through the same guard as webhooks: names
  that resolve to private, loopback or link-local addresses are refused, so
  a LAN-only name can only use the TXT record.

Press **Verify**. Pending names are also re-checked hourly for a week, and
verified names daily; after **three failed checks in a row** the name stops
being served until it verifies again. DNS lookups have an 8-second timeout
and only read TXT records.

Rules:

- Host names are matched exactly, ignoring case, the port and a trailing dot.
  `docs.example.com` doesn't cover `www.docs.example.com` or `example.com`;
  there are no wildcards.
- Atelier's own host name (`ATELIER_PUBLIC_URL`, which must be set to use
  custom domains), the Pages host and the tunnel's host names can't be
  claimed. A name another site has verified can't be claimed either.
- A name that isn't verified is an ordinary request to Atelier, as before.
- The custom domain serves the site and nothing else: no app pages, git,
  sign-in or app cookies. Responses carry the same headers as on the Pages
  host, except that the sandbox keeps the page's origin
  (`allow-same-origin`): the domain belongs to this one site, and private
  sites need their own requests to carry their cookie.

### TLS

Atelier doesn't terminate TLS. Pick the route on the settings page; it shows
these steps with your names filled in.

**Nginx Proxy Manager**

1. DNS: point `docs.example.com` (an A or CNAME record) at the machine running
   Nginx Proxy Manager, and add the TXT record.
2. Proxy Hosts → Add Proxy Host:
   - Domain names: `docs.example.com`
   - Scheme `http`, forward hostname/IP and port: the same as the app's proxy
     host (the Atelier container, port `3000` on a shared Docker network or
     `3010` on the host in `deploy/portainer/stack.yml`).
   - Don't add anything that rewrites the `Host` header.
3. SSL tab: request a Let's Encrypt certificate (the name must resolve
   publicly, or use a DNS challenge), tick Force SSL and HTTP/2 Support.
4. Press **Verify**.

**The named Cloudflare tunnel** (for names on a Cloudflare zone)

1. Remote access (Settings → Remote access) uses a **named tunnel**, with
   **Pages** ticked under what's exposed.
2. Cloudflare Zero Trust → Networks → Tunnels → your Atelier tunnel →
   Public Hostname → Add: hostname `docs.example.com`, service type `HTTP`,
   URL the remote listener (`ATELIER_TUNNEL_LISTEN`, `127.0.0.1:3001`, the
   same service as the tunnel's other hostnames). Cloudflare creates the DNS
   record and the certificate.
3. Add the TXT record in Cloudflare DNS, then press **Verify**.

## Private repositories

On the shared Pages host only public repositories are served. A private site
there would need a sign-in on that host, and every site shares it: any
published site's script could then read a private site with the visitor's
credentials. So a site goes offline there the moment its repository is made
private.

A **verified custom domain** is the site's own origin, so a private
repository can publish once it has one, and is served only there, only to
people who can read the repository:

1. A visitor without a site session is redirected to the app,
   `/-/pages/signin?host=<name>&return=<path>`, which needs an ordinary
   Atelier sign-in (a passkey through the tunnel) and read access.
2. The app redirects back to
   `https://<name>/.well-known/atelier-pages/_signin?token=…` with a random
   token stored hashed, bound to that name and person, valid for one minute
   and usable once.
3. The site host checks the token and read access again and sets its own
   cookie: host-only (no `Domain`), `Path=/`, `HttpOnly`, `SameSite=Lax`, and
   `Secure` with the `__Host-` prefix over HTTPS. It lasts 8 hours.
4. Every request re-checks the session and read access, so losing access
   counts at once. Private responses are `Cache-Control: private` and never
   carry `Access-Control-Allow-Origin`.
   `/.well-known/atelier-pages/_signout` signs out; changing or removing the
   domain signs everyone out and takes the site offline.

The app's session cookie never reaches the custom domain, and the site's
cookie means nothing to the app.

## Pull request previews

With previews on (the default; **Settings → Pages → Pull request previews**),
each open pull request's site is published at

    <pages>/~preview/<owner>/<repo>/<number>/

`~preview` sits where an owner name goes, and owner names must start with a
letter or digit, so the address can't collide with any site or any file in a
site (a path such as `<pages>/<owner>/<repo>/_preview/` could). Links in the
site should be relative for the preview to work.

- **Branch source:** the configured folder is published from the pull
  request's head when it opens or reopens and on every push to its branch.
- **Actions source:** a `pull_request` run's `atelier/deploy-pages` step
  uploads the preview.
- One comment on the pull request carries the link, edited in place on every
  push (and when a build fails, which keeps the previous preview up).
- Closing or merging the pull request removes the preview, and the comment
  says so. Turning previews off removes them all.
- **Pull requests from anyone but the owner** (imported, or from bots) wait,
  like their Actions runs: a branch-source preview is published once an admin
  presses **Approve and publish** in Settings → Pages, for that head commit
  only (a later push waits again); an Actions job only runs after approval,
  and an upload from a job still waiting is refused.
- Previews have the same size limits and snapshot handling as deploys and
  never become the live site or a rollback target. Each pull request keeps
  only its current preview, and at most `ATELIER_PAGES_KEEP` previews are
  kept per repository, separately from the site's deploys; the least
  recently updated goes first (its comment says so).
- Only public repositories get previews.
