# Atelier behind Nginx Proxy Manager

Use this instead of `deploy/nginx/atelier.conf` when your proxy is Nginx Proxy Manager (NPM).

## Proxy host

**Hosts → Proxy Hosts → Add Proxy Host**

| Field | Value |
|---|---|
| Domain names | `atelier.<your-domain>` |
| Scheme | `http` |
| Forward hostname / IP | your server's IP (or `atelier` if NPM and Atelier share a Docker network) |
| Forward port | `3010` (or `3000` on a shared network) |
| Websockets support | on |
| Block common exploits | on |

**SSL tab:** pick or request a certificate, and turn on Force SSL and HTTP/2.

## Advanced tab

Paste this into *Custom Nginx Configuration*. It lets big pushes through, streams git data instead of buffering it, and passes the headers Atelier needs to build `https://` clone URLs.

```nginx
client_max_body_size 0;
proxy_buffering off;
proxy_request_buffering off;
proxy_read_timeout 600s;
proxy_send_timeout 600s;
proxy_set_header X-Forwarded-Host $host;
```

## Atelier settings

In the Portainer stack, set:

- `ATELIER_PUBLIC_URL` to `https://atelier.<your-domain>`, exactly what you type in the browser.
- `ATELIER_TRUST_PROXY` to `"true"`.
