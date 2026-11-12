# Runners

`atelier-runner` is a single static binary that runs Actions jobs on a device. It only connects out to your server, so laptops need no open ports. It needs `git`; Docker only for `container:` jobs and docker actions, and Node.js only for JavaScript actions.

```sh
make runner          # bin/atelier-runner for this machine
make runner-all      # macOS, Linux and Windows builds (arm64 and amd64) in bin/
```

1. Create a registration token under **Settings → Runners**, then register the device once:

   ```sh
   atelier-runner register --url https://git.example.com --token <token> \
     --name MacBook --labels mac --capabilities jobs,docker,discovery
   ```

   Labels always include `self-hosted`, the OS (`macos`, `linux`, `windows`) and the CPU (`x64`, `arm64`). Capabilities: `jobs` (default), `docker` (allow `container:` jobs, `docker://` steps and docker actions), `discovery` (report git repos found under `~/Projects`, or `%USERPROFILE%\source` and `~\Projects` on Windows, every 10 minutes, with credentials stripped from remote URLs; change the folders with `--discovery-paths`). The config, including the runner token, is saved with mode 0600 to `<user config dir>/atelier-runner/config.json` (`--config` overrides it).
2. Try it in the foreground with `atelier-runner run` (`--parallel N` runs several jobs at once; `--grace 5m` lets a running job finish on Ctrl-C/SIGTERM before it is cancelled).
3. Install it as a service: `atelier-runner install` (a LaunchAgent on macOS, a systemd user unit on Linux, or `--system` for a system unit, a Windows service from an Administrator prompt). `--dry-run` prints the file and commands without changing anything. `atelier-runner status` shows the config and service state; `atelier-runner uninstall` removes it.

Jobs run in a fresh workspace that is deleted afterwards. `run:` steps use bash (falling back to sh) on macOS/Linux and pwsh, then powershell, then cmd on Windows, unless `shell:` says otherwise (`bash`, `sh`, `pwsh`, `powershell`, `cmd`, `python`). The usual `GITHUB_*` variables (plus `ATELIER_*` equivalents), `GITHUB_OUTPUT`/`GITHUB_ENV`/`GITHUB_PATH`/`GITHUB_STEP_SUMMARY` and workflow commands such as `::add-mask::` work like on GitHub, so existing workflows run unchanged. `actions/checkout` is built in. Other actions are downloaded from GitHub at the exact commit in `.atelier/actions.lock`, checked against that commit and cached. Secrets, the job token and anything passed to `::add-mask::` show as `***` in logs.

On the Portainer host you can run the runner as a container instead: `make runner-docker` builds `atelier-runner:latest`. Register with `docker run --rm -it -v atelier-runner:/runner atelier-runner register --config /runner/config.json --work-dir /srv/atelier-runner/work …`, then run it with `-v atelier-runner:/runner -v /srv/atelier-runner/work:/srv/atelier-runner/work -v /var/run/docker.sock:/var/run/docker.sock`. The work dir must have the same path inside and outside the container, because Docker resolves job bind mounts on the host.
