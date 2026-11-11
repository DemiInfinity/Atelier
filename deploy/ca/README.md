# Extra trusted certificates

Put the **public** root certificate of a private certificate authority here
(for example mkcert's `rootCA.pem`, saved as `homelab.crt`) when Atelier's
address uses a certificate your own CA issued. Both Docker images add every
`*.crt` in this folder to their trusted certificates, so the server-side runner
can clone over HTTPS and agent jobs can reach `/mcp`.

Never put a private key here (`rootCA-key.pem` stays on the machine that
created it). `*.crt` files in this folder are ignored by git; copy the file to
the server's checkout instead of committing it.
