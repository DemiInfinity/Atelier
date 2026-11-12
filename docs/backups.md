# Backups and restore

Atelier takes a snapshot every day at `ATELIER_BACKUP_TIME` (default `03:00`, server local time; `off` disables it), and whenever you click **Back up now** under **Settings → Backups and storage**. A snapshot is one `atelier-<time>.tar.gz` holding a consistent copy of the database (`VACUUM INTO`), a `git bundle` of every repository and a `manifest.json` with sizes and checksums. Snapshots go to `ATELIER_BACKUP_DIR` (default `<data>/backups`; the stack mounts a separate `atelier-backups` volume at `/backups`) and, if `ATELIER_BACKUP_S3_ENDPOINT`, `_BUCKET`, `_ACCESS_KEY` and `_SECRET_KEY` are set, to any S3-compatible bucket too (`_PREFIX` and `_REGION` are optional). Retention keeps 14 daily, 8 weekly and 6 monthly snapshots (`ATELIER_BACKUP_KEEP_DAILY`/`_WEEKLY`/`_MONTHLY`) in both places. The settings page shows history, lets you download snapshots and shows disk usage. `atelier backup` takes one from the shell.

Restore from the server's shell. Whatever is replaced is moved aside to `pre-restore-<time>`, never deleted:

```sh
# one repository; Atelier can keep running
docker exec -it atelier atelier restore /backups/atelier-20261110-030000.tar.gz --repo demi/atelier
# everything; stop Atelier first (restore refuses while it runs unless you pass --force)
docker stop atelier
docker run --rm -v atelier_atelier-data:/data -v atelier_atelier-backups:/backups atelier:latest restore /backups/atelier-20261110-030000.tar.gz
docker start atelier
```
