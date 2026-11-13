# ms365backupd – notes for Claude

Go daemon that backs up Microsoft 365 (OneDrive, SharePoint, mail, contacts, calendar)
incrementally via Microsoft Graph to an external disk (`<target>/ms365backupd/`). Targets
Raspberry Pi (linux/arm64, armv7) and macOS. User docs: README.md – keep it in sync with
every user-visible change (flags, config keys, TUI keys, setup hints).

## Build & test

```sh
make               # help: every target with a `## description` comment is listed
make check         # gofmt check + vet + tests (what CI runs)
make build build-pi build-pi32 dist
```

New make targets need a `## description` comment so `make help` lists them.

## Branches & releases

- Work on `develop`; `main` is released code. Repo: github.com/ulbrich/ms365backupd
  (Go module path `github.com/ulbrich/ms365backupd`).
- `make release` (script/release.sh) runs on a clean `main` in sync with origin: tags
  `vx.y.z`, pushes the tag, waits for `.github/workflows/release.yml` (make check, make dist,
  `gh release create`). `make next-version` previews the tag. Never release without being asked.

## Layout

- `cmd/ms365backupd` – main
- `internal/cli` – cobra commands, global flags (`root.go`), service files (`service.go`)
- `internal/config` – config.json load/merge/validate/save, defaults, Graph scopes
- `internal/auth` – OAuth2 device code flow, `token.json`, refresh-token rotation
- `internal/graph` – Graph HTTP client (paging, delta, throttling, errors)
- `internal/backup` – engine: drive.go (OneDrive/SharePoint), mail, contacts, calendar
- `internal/daemon` – scheduler, retries, disk polling, unix-socket API, SIGHUP reload
- `internal/tui` – bubbletea dashboard (`tui.go`) and setup wizard (`setup.go`)
- `internal/store` (bbolt sync state, per disk), `disk`, `exclude`, `history`, `notify`,
  `quickxor`, `status`

## Conventions / decisions

- **Tenant:** the config key is `tenant_id`, the CLI flag `--tenant-id`, the TUI field
  "Tenant ID". It is required and has **no default** (no `common`): it is the Directory
  (tenant) ID GUID (or primary domain). `oauth2.AzureADEndpoint("")` silently falls back
  to `common`, so auth must reject an empty tenant explicitly.
- **SharePoint:** `sharepoint_sites` defaults to `["*"]` (`config.AllSites`): root site +
  sites of the user's M365 groups (`me/memberOf` → `groups/{id}/sites/root`). Explicit
  entries are added, deduplicated by site id. `null`/missing in the file means `["*"]`,
  `[]` means none; `Save` writes empty lists as `[]`, never `null`.
- Scopes: `Files.Read.All` covers SharePoint; do not add `Sites.Read.All`.
- Only write below `<target>/ms365backupd/`; sync state lives locally in the state dir,
  per disk id.
- Daemon reloads config on SIGHUP / `POST /reload`; a running backup finishes first.
- The setup wizard writes only on save; cancel must leave the file untouched.
  Keys of the config without a wizard field must survive a save.

## Raspberry Pi image (`make image`)

- `script/pi-image.sh` runs on the host (macOS has **bash 3.2**: no `[[ -v ]]`, no
  `declare -A`, no `${x,,}`), reads `pi-image/env`, validates, then runs
  `script/pi-image/build.sh` in a privileged `debian:trixie` container. `build.sh` grows,
  mounts and shrinks the image and writes cloud-init `user-data`/`network-config` to the boot
  partition; `script/pi-image/provision.sh` runs in the arm64 chroot (packages, user, sshd,
  avahi, systemd units, motd).
- A new setting must be added in four places: the `settings` list and defaults in
  `pi-image.sh`, the `docker run -e` list, `pi-image/env.example`, and the README table.
  Secrets are passed by name (`-e VAR`), never as values on a command line.
- `pi-image/env` and `pi-image/ssh/` are private (WiFi password, keys): never commit, print or
  quote them; `env.example` uses only generic example values. Built images contain the WiFi
  PSK and password hash.
- The motd in `provision.sh` and the README sections *New Raspberry Pi: step by step* and
  *Raspberry Pi troubleshooting* describe the same first steps; keep them in sync.
- A full build takes long (downloads, full-upgrade, emulation on x86_64). For script-only
  changes, test with a stub `docker` and `bash` 3.2, and `sshd -t` for sshd snippets.

### Helping on a running Pi

Known failure causes, in order of likelihood:
- `target is not a mount point`: `/etc/fstab` UUID does not match `sudo blkid` (disk was
  reformatted). Fix the line, `systemctl daemon-reload`, `mount /mnt/backup`, and `chown` a
  new ext4 root to the user.
- `kex_exchange_identification: Connection closed`: OpenSSH `PerSourcePenalties`; wait, or
  set `PerSourcePenaltyExemptList` (`PI_SSH_TRUSTED_NETS`).
- Unexpected reboots / I/O errors: undervoltage from the USB SSD (`vcgencmd get_throttled`,
  `journalctl -b -1`).
Change system files on a user's Pi only with their explicit OK, and back them up first
(e.g. `/etc/fstab.bak`).

## Working in this repo

- The Bash sandbox blocks reading this directory; commands need the sandbox disabled.
- When editing with scripted replacements, assert that the old text matched (a silent
  non-match once caused the s/l TUI keys to be missing).
- Commit only when asked.
