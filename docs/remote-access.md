# Remote access (tunnel)

**Settings → Remote access** reaches your Atelier from outside the home network through Cloudflare Tunnel or Tailscale, with no port forwarding. It is **off by default**; a new install or a full restore is always off. While it's on, a banner across the app says so and has a **Turn off** button. Turning it off kills the helper (`cloudflared`, or `tailscaled` and `tailscale serve`/`funnel`), closes every remote connection and signs out every session started remotely. From the shell: `docker exec atelier atelier tunnel off` (and `atelier tunnel status` says which provider and mode).

- **Quick tunnel:** no account. You get a random `https://….trycloudflare.com` address that changes every time it starts.
- **Named tunnel:** a stable hostname on your own domain. Create a tunnel in Cloudflare Zero Trust, paste its token (stored encrypted, and re-encrypted by `atelier secrets rotate-key`), and point the public hostname's service at `http://127.0.0.1:3001`, never port 3000.

- **Tailscale, Private:** the studio joins your tailnet as a machine (default name `atelier`) and is served at `https://atelier.<your-tailnet>.ts.net` to your own devices only (`tailscale serve`). The address never changes.
- **Tailscale, Public (Funnel):** the same address, open to the internet (`tailscale funnel`). This is what Atelier Connect and inbound webhooks need.

Setting up Tailscale, once:

1. In the Tailscale admin console → **DNS**: turn on MagicDNS and press **Enable HTTPS**.
2. For Public only → **Access controls**: give the machine the `funnel` node attribute, e.g. `"nodeAttrs": [{"target": ["autogroup:member"], "attr": ["funnel"]}]` (target the tag instead if you tag the key).
3. **Settings → Keys → Generate auth key**: one-off (not reusable), **not ephemeral** (an ephemeral machine is deleted while remote access is off), pre-approved if your tailnet uses device approval. A tag such as `tag:atelier` is optional; a tagged machine's key doesn't expire. Without a tag, open the machine afterwards and choose **Disable key expiry**, or it has to sign in again after 180 days.
4. In Atelier → Settings → Remote access: choose Tailscale, Private or Public, paste the key, Save, then **Turn on remote access**. The key is stored encrypted until the machine has joined, then deleted; it is never shown again.
5. Create the one-time passkey link and open it on a device that can reach the address. The address is stable, so that passkey keeps working.

`tailscaled` runs inside the container with userspace networking as the unprivileged `atelier` user: no `/dev/net/tun`, no `NET_ADMIN`, no extra ports. Off stops it completely (the machine shows offline in the admin console); on brings it back in a few seconds. Its identity lives in `/data/tailscale` (mode 0700) and is **left out of backups**: after restoring onto another machine, paste a new auth key (and remove the old machine in the admin console). **Forget this node** signs it out and deletes that state. Being on your tailnet never signs anyone in to Atelier: the who's-connected log shows the tailnet login of the device, and a passkey is still required. If HTTPS or Funnel isn't enabled for the tailnet, the status card says what to switch on and carries on by itself once you have. Pages are not served over Tailscale (they need a hostname of their own).

Remote requests get only what you expose: web UI and git over HTTPS by default, MCP, Pages and the API if you tick them, and never the registries. Everything else is a 404. Remotely, browsers sign in with passkeys only; your password is refused. Git and APIs need access tokens, even for public repos. A passkey belongs to one address, so at home you create a one-time link that lets the remote browser add its own passkey. The page also logs who connected (time, IP, country, what, signed in) for 7 days. `ATELIER_TUNNEL_LISTEN` (default `127.0.0.1:3001`), `ATELIER_CLOUDFLARED` (default `cloudflared`), `ATELIER_TAILSCALE` and `ATELIER_TAILSCALED` (default `tailscale` and `tailscaled`; all three binaries are in the image) rarely need changing.
