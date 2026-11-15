# Extra trusted certificates

Put the **public** root certificate of a private certificate authority here
(for example mkcert's `rootCA.pem`, saved as `homelab.crt`) when Atelier's
address uses a certificate your own CA issued. Both Docker images add every
`*.crt` in this folder to their trusted certificates, so the server-side runner
can clone over HTTPS and agent jobs can reach `/mcp`.

Never put a private key here (`rootCA-key.pem` stays on the machine that
created it). `*.crt` files in this folder are ignored by git; copy the file to
the server's checkout instead of committing it.

## Images built by the pipeline

`.atelier/workflows/image.yml` builds both images from a fresh checkout, which
has no `*.crt` here (git ignores them). So the pipeline writes the repository
variable **`EXTRA_CA_CERTS`** into this folder before building. Set it once in
Atelier → this repository → Settings → Variables: paste the PEM text of the
public root certificate (several certificates one after another are fine).

```bash
cat "$(mkcert -CAROOT)/rootCA.pem"    # the text to paste
```

Without it the pipeline warns and the images trust only public CAs: a runner
then fails every poll with `x509: certificate signed by unknown authority` and
can't run the pipeline that would fix it. To recover a runner in that state,
copy the certificate into the running container and restart it (a restart keeps
the copy; recreating the container loses it until the next pipeline image):

```bash
docker cp homelab.crt atelier-runner:/usr/local/share/ca-certificates/homelab.crt
docker exec -u 0 atelier-runner update-ca-certificates && docker restart atelier-runner
```
