# MS365Backupd

A small, robust daemon that backs up a Microsoft 365 account (OneDrive, SharePoint
document libraries, Exchange mail, contacts and calendars) **incrementally** to an external
USB disk. Additional mailboxes (other Microsoft 365 accounts or any IMAP server) can be
backed up alongside.

**Warning:** This sideproject was implemented mainly using AI tools. Use it with a bigger
grain of salt than with other open source projects you're using.

The tool is built with a Raspberry Pi in mind, but also runs on e.g. macOS. It is a
single static Go binary with no runtime dependencies.

- Writes only below `<target>/ms365backupd/`. Everything else on the disk is left alone.
- Incremental backups use Microsoft Graph delta queries: only changes are fetched, renames
  and moves are applied on disk instead of re-downloading, and deletions are kept in
  `.deleted/<date>/`.
- A newly connected disk is detected by an identity marker and automatically gets a full
  backup. Reconnecting a known disk continues incrementally.
- Sends an email for every failed daily backup (after retries), plus a weekly statistics report.
- Configured from `~/.config/ms365backupd/config.json`. Every setting can be overridden by a flag.
- Shell completion for bash, zsh and fish, and a TUI dashboard with a guided setup screen.

## Why not an existing tool?

| Project | Status |
|---|---|
| [Corso](https://github.com/alcionai/corso) (open source, M365 → local/S3) | Archived in Oct 2024, no longer maintained ([issue tracker](https://issues.ecosyste.ms/hosts/GitHub/repositories/alcionai%2Fcorso)) |
| [CubeBackup for Microsoft 365](https://platform.softwareone.com/product/cubebackup-for-microsoft-365/PCP-1988-2481) | Commercial, server-oriented |
| [CodeTwo Backup for Office 365](https://www.codetwo.com/freeware/backup-for-office-365-download/) | Windows only |
| rclone (OneDrive backend) | Good for files, but no mail, contacts or calendar. A plain mirror has no deletion history or new-disk handling |

rsync cannot talk to Microsoft 365, so this tool uses the Graph API directly. Graph's delta
queries give the incremental behaviour that rsync would otherwise provide.

## Disk layout

```
<target>/ms365backupd/
  .ms365backupd/disk.json         disk identity (UUID, account) – do not delete
  onedrive/…                      OneDrive, original folder structure and timestamps
  sharepoint/<site>/<library>/…   optional SharePoint document libraries
  mail/<folder>/<year>/<date>_<subject>_<id>.eml   one RFC 822 file per message (MIME)
  contacts/<folder>/*.json + *.vcf
  calendar/<calendar>/*.json      full Graph event objects (UTC)
  mailboxes/<name>/<folder>/<year>/*.eml   additional mailboxes (Graph or IMAP)
  .deleted/<YYYY-MM-DD>/…         items deleted/replaced in M365 (keep_deleted)
```

The sync state (delta tokens, item index) lives on the local machine in
`~/.local/state/ms365backupd/disks/<disk-id>/`, one database per disk. A fresh disk therefore
always starts from the beginning. Keeping the database off the disk also keeps it safe from
exFAT and unplugging issues.

## 1. Register an app in Entra ID (once)

1. Go to <https://entra.microsoft.com> → *App registrations* → *New registration*.
   Choose a name such as `ms365backupd` and *Accounts in this organizational directory only*
   (single tenant). No redirect URI is needed.
2. Under *Authentication* → *Advanced settings*, set **Allow public client flows** to **Yes**
   and save. This is required for the device code login; without it the token request fails
   (typically `AADSTS7000218` / `invalid_client`).
3. Under *API permissions*, add Microsoft Graph **delegated** permissions:
   `User.Read`, `offline_access`, `Files.Read.All`, `Mail.Read`, `Contacts.Read`, `Calendars.Read`.
   Add `Mail.Send` if notifications are sent through the account itself
   (`email.via = "graph"`). `Files.Read.All` also covers SharePoint, so `Sites.Read.All`
   is not needed. Grant admin consent if your tenant requires it.
4. On the app's *Overview* page copy the **Application (client) ID** and the
   **Directory (tenant) ID**. Both are GUIDs; both are required. The tenant ID has no
   default: the generic `common` endpoint does not work for single-tenant apps.

## 2. Install

Download a binary from the [releases](https://github.com/ulbrich/ms365backupd/releases)
(`linux-arm64` for 64-bit Raspberry Pi OS, `linux-armv7` for 32-bit, `darwin-arm64` for
Apple Silicon), or build it yourself:

```sh
make build-pi        # bin/ms365backupd-linux-arm64 (64-bit Raspberry Pi OS)
make build-pi32      # bin/ms365backupd-linux-armv7
scp bin/ms365backupd-linux-arm64 pi@raspberrypi:/tmp/ms365backupd
ssh pi@raspberrypi 'sudo install -m755 /tmp/ms365backupd /usr/local/bin/'
```

Mount the SSD with `nofail` (see `deploy/systemd/fstab.example`). ext4 is recommended. exFAT
works if the disk also has to be readable on a Mac.

### Ready-made Raspberry Pi image

`make setup` installs what is needed to build and flash it (Go, xz, Docker, Raspberry Pi
Imager; Homebrew on macOS, apt on Debian/Ubuntu). `make image` builds a headless Raspberry Pi OS Lite (64-bit, Pi 3/4/5/Zero 2) image with
ms365backupd installed as an enabled systemd service. Only the configuration is missing: the
service starts once `~/.config/ms365backupd/config.json` exists. It needs Docker (Linux or
Docker Desktop on macOS). On x86_64 hosts it registers arm64 emulation once
(`tonistiigi/binfmt`), and the build is much slower there. The result is
`bin/ms365backupd-rpi-arm64-<version>.img.xz`; see *Flashing* below.

Preinstalled: openssh-server, tailscale, git, gh, curl, rsync, glow, ack, ripgrep, fd, jq, fzf,
bat, tree, htop, ncdu, neovim, pandoc with weasyprint (PDF), Node.js LTS, Python 3 with pip and
uv, [Claude Code](https://claude.com/claude-code) (`claude`), [pi](https://pi.dev) (`pi`),
[herdr](https://herdr.dev) (`herdr`) and the [Open Knowledge](https://openknowledge.sh) CLI
(`okn`). The download of Raspberry Pi OS is cached in `.cache/pi-image/`.

```sh
make image PI_USER=jan                       # SSH keys from pi-image/ssh/authorized_keys, generated password
make image PI_PASSWORD=secret WIFI_SSID=Home WIFI_PASSWORD=… WIFI_COUNTRY=DE
```

The settings can also be kept in `pi-image/env` (shell syntax, `KEY="value"`; not committed,
as it holds the WiFi password). Variables on the command line override the file;
`PI_IMAGE_ENV=<file>` reads another one:

```sh
WIFI_SSID="Home"
WIFI_PASSWORD="…"
WIFI_COUNTRY="DE"
MDNS_HOSTNAME="foo"                          # ssh foo.local
```

| Variable | Default | |
|---|---|---|
| `PI_USER` | `pi` | login user (passwordless sudo) |
| `PI_SSH_KEY` | `pi-image/ssh/authorized_keys`, else the first of `~/.ssh/id_{ed25519,ecdsa,rsa}.pub` | file with the public key(s) for SSH; with a key, SSH password login is off |
| `PI_PASSWORD` | generated | password for the console (and SSH if there is no key). A generated one is printed at the start and end of the build |
| `PI_HOSTNAME` | `ms365backup` | host name; announced as `<hostname>.local` unless `MDNS_HOSTNAME` is set |
| `PI_SSH_TRUSTED_NETS` | none | comma-separated addresses or CIDR networks, e.g. `192.168.1.0/24,fd00:1234:5678::/64`, that OpenSSH never blocks. Since OpenSSH 9.8, `sshd` refuses an address for up to 10 minutes after logins from it time out or fail (`PerSourcePenalties`), which looks like the Pi is unreachable (`kex_exchange_identification: Connection closed by remote host`) |
| `MDNS_HOSTNAME` | `PI_HOSTNAME` | Bonjour/mDNS name without `.local`: `foo` makes the Pi reachable as `foo.local` (`ssh foo.local`). SSH is also announced as a service (`dns-sd -B _ssh._tcp`) |
| `PI_TIMEZONE` | the host's | e.g. `Europe/Berlin` |
| `WIFI_SSID`, `WIFI_PASSWORD`, `WIFI_COUNTRY` | none | WiFi; Ethernet works without settings |
| `TS_AUTHKEY` | none | Tailscale auth key: joins the tailnet on first boot, then the key is deleted. Use a one-off key; otherwise run `sudo tailscale up` |
| `NODE_MAJOR` | `26` | Node.js major version (NodeSource) |
| `IMAGE_UPGRADE` | `1` | `0` skips upgrading the base image's packages |
| `IMAGE_COMPRESS` | `1` | `0` writes an uncompressed `.img` |
| `PI_IMAGE_URL` | latest Raspberry Pi OS Lite arm64 | base image (`.img.xz`) |

The WiFi settings land in `network-config` and the rest of the first-boot settings in
`user-data` on the boot partition (cloud-init), so they can still be edited after flashing.
After the first login, the message of the day lists the remaining steps: mount the disk,
run `ms365backupd` for the setup, `sudo systemctl start ms365backupd`.

**Flashing** (SD card of 8 GB or more). Optionally verify the image first:
`cd bin && shasum -a 256 -c ms365backupd-rpi-arm64-*.img.xz.sha256`.

- Raspberry Pi Imager: choose your Pi as *Device*, *Use custom* (at the bottom of the
  *Operating System* list) and the `.img.xz`, then the SD card. Answer **No** to OS
  customisation: it would replace the user, keys and settings set up in the image.
- Or on the command line (macOS; on Linux use `lsblk`, `/dev/sdX` and `bs=4M`). `dd`
  overwrites the disk you name without asking, so check the number and size:

  ```sh
  diskutil list                       # find the SD card, e.g. /dev/disk4
  diskutil unmountDisk /dev/disk4
  xzcat bin/ms365backupd-rpi-arm64-*.img.xz | sudo dd of=/dev/rdisk4 bs=4m status=progress
  diskutil eject /dev/disk4
  ```

The first boot takes a minute or two longer: the filesystem grows to fill the card, and
cloud-init sets the hostname and time zone and creates the SSH host keys. Then log in with
`ssh <PI_USER>@<MDNS_HOSTNAME or PI_HOSTNAME>.local` (e.g. `ssh pi@ms365backup.local`) and the
key; the password is for the console and `sudo`. After flashing again, the Pi has new host keys: remove the old
one with `ssh-keygen -R ms365backup.local`. Without `TS_AUTHKEY`, run `sudo tailscale up`.

**Finding the Pi** if `.local` does not resolve (e.g. mDNS blocked in a guest network):
`dns-sd -B _ssh._tcp` (macOS) or `avahi-browse -rt _ssh._tcp` (Linux) lists SSH hosts that are
announced via mDNS; `dns-sd -G v4 foo.local` resolves one name. Otherwise look for the host name in the
router's DHCP list, or for a Raspberry Pi MAC address (`b8:27:eb`, `dc:a6:32`, `e4:5f:01`,
`d8:3a:dd`, `2c:cf:67`, `88:a2:9e`) in `arp -a` after `nmap -sn 192.168.1.0/24`. Without WiFi
settings in the image, the Pi needs Ethernet.

### New Raspberry Pi: step by step

What you need: a Pi 3/4/5 (a Pi 4 or 5 for a USB SSD), its **official power supply** (a USB
SSD on a weak supply causes undervoltage, I/O errors and resets), an SD card of 8 GB or more,
the backup SSD, and the app registration from step 1.

1. **Build the image** on your Mac or Linux machine (once: `make setup`, and start Docker
   Desktop):

   ```sh
   cp pi-image/env.example pi-image/env     # set WIFI_*, optionally MDNS_HOSTNAME, PI_SSH_TRUSTED_NETS
   mkdir -p pi-image/ssh && cat ~/.ssh/id_ed25519.pub >> pi-image/ssh/authorized_keys   # optional
   make image                               # note the generated password it prints
   ```

2. **Flash** `bin/ms365backupd-rpi-arm64-<version>.img.xz` with Raspberry Pi Imager (*Use
   custom*, then **No** to OS customisation) or `dd`, see *Flashing* above.
3. **Boot and log in.** Insert the card, connect the SSD (on a Pi 4 to a blue USB 3 port) and
   power on. After 2–3 minutes: `ssh pi@ms365backup.local` (if you flashed a Pi before under
   that name: `ssh-keygen -R ms365backup.local` first). The message of the day lists the
   remaining steps.
4. **Prepare the backup disk.** Find it with `lsblk -f` (usually `/dev/sda`). For a new or
   empty disk, create one ext4 partition. This **erases the disk**:

   ```sh
   sudo wipefs -a /dev/sda
   echo ',,L' | sudo sfdisk --label gpt /dev/sda
   sudo mkfs.ext4 -L m365backup /dev/sda1
   ```

5. **Mount it at `/mnt/backup`** by UUID, with `nofail` so the Pi boots without the disk:

   ```sh
   uuid=$(sudo blkid -s UUID -o value /dev/sda1)
   echo "UUID=$uuid /mnt/backup ext4 defaults,noatime,nofail,x-systemd.device-timeout=10s 0 2" | sudo tee -a /etc/fstab
   sudo systemctl daemon-reload
   sudo mount /mnt/backup
   sudo chown "$USER": /mnt/backup          # a new ext4 filesystem belongs to root
   findmnt /mnt/backup                      # must show /dev/sda1
   ```

   Only one line in `/etc/fstab` may mount `/mnt/backup`; replace an old one instead of adding
   a second. For exFAT see `/usr/local/share/doc/ms365backupd/fstab.example`.
6. **Configure and sign in:** run `ms365backupd`. The setup screen opens; enter the tenant
   ID, client ID and `/mnt/backup` as target, save with ctrl+s, then press **l** and sign in
   with the code on any device's browser (see *3. Configure*).
7. **Start the service** (it is already enabled and starts on every boot from now on):

   ```sh
   sudo systemctl start ms365backupd
   ms365backupd status                      # disk "available", backup running
   journalctl -u ms365backupd -f            # follow the log
   ```

   The first full backup can take hours to days, depending on the amount of data. It
   continues after a reboot or reconnecting the disk.
8. **Optional:** `sudo tailscale up` for remote access, `ms365backupd test-email` to check
   notifications, `ms365backupd tui` for the dashboard.

### Raspberry Pi troubleshooting

- **`target is not a mount point (disk not connected?)`** although the SSD is plugged in: the
  UUID in `/etc/fstab` does not match the disk, typically after reformatting it (a new
  filesystem gets a new UUID). Compare `sudo blkid` with `grep backup /etc/fstab`, fix the
  line, then `sudo systemctl daemon-reload && sudo mount /mnt/backup` and, for a new ext4
  filesystem, `sudo chown "$USER": /mnt/backup`. The daemon notices the disk by itself.
- **`permission denied` writing to `/mnt/backup`**: the filesystem root belongs to root; run
  `sudo chown "$USER": /mnt/backup` while the disk is mounted.
- **`kex_exchange_identification: Connection closed by remote host`** on `ssh`: OpenSSH blocks
  an address for a few minutes after failed or timed-out logins (`PerSourcePenalties`). Wait
  up to 10 minutes. To prevent it, build the image with `PI_SSH_TRUSTED_NETS`, or on a running
  Pi:

  ```sh
  echo 'PerSourcePenaltyExemptList 192.168.1.0/24' | sudo tee /etc/ssh/sshd_config.d/50-trusted-nets.conf
  sudo sshd -t && sudo systemctl reload ssh
  ```

- **`REMOTE HOST IDENTIFICATION HAS CHANGED`** after flashing again: `ssh-keygen -R
  ms365backup.local` (and the IP address, if you used it).
- **Did the Pi reboot or the daemon restart?** `uptime -s` and `journalctl --list-boots` show
  the boots, `systemctl show ms365backupd -p ActiveEnterTimestamp -p NRestarts` the daemon's
  start and restarts. `journalctl -b -1 -n 50` shows the end of the previous boot; if it just
  stops, power was lost. `vcgencmd get_throttled` other than `0x0` means undervoltage or
  overheating: use the official power supply, or a powered USB hub or an SSD with its own
  power.
- **The backup does not start:** `systemctl status ms365backupd`. The service only starts once
  `~/.config/ms365backupd/config.json` exists; `ms365backupd config validate` checks it and
  `ms365backupd status` shows the disk and sign-in state.

## 3. Configure

The easiest way is the setup screen:

```sh
ms365backupd                 # first start without a config file opens the setup screen
ms365backupd tui --setup     # later: or press s in the dashboard
```

It walks through Account → Disk → Data → Mailboxes → Schedule → Email → Save, shows what is already
configured (`default`, `● changed (was …)`), validates every field and checks the disk
live. It writes `~/.config/ms365backupd/config.json` (mode 0600) only when you choose
*save* (enter on the last step, or ctrl+s anywhere); esc discards. Settings without a field
(`poll_interval`, `state_dir`, `log_file`, `graph_url`) are kept as they are. After saving,
press **l** to sign in right in the TUI.

Keys: tab/shift+tab or ↑/↓ move between fields, ←/→ or space change toggles and choices,
pgdn/pgup (ctrl+n/ctrl+p) switch steps. In the exclude box enter adds a line and tab leaves it.
In the Mailboxes step: **a** add, **d** remove, **space** enable/disable, **l** sign in
(graph mailbox), **t** test the login; ↑/↓ select.

Without the TUI:

```sh
ms365backupd config init --tenant-id <directory-tenant-id> --client-id <application-client-id> --target /mnt/backup
$EDITOR ~/.config/ms365backupd/config.json
ms365backupd config validate
ms365backupd login           # shows a code; sign in on any device's browser
ms365backupd test-email
```

### Sign-in and token renewal

Microsoft 365 has no personal access tokens. `login` uses the OAuth 2.0 device code flow
(delegated, as you). Entra ID returns a short-lived access token (~1 h) and a refresh token,
both stored in `~/.config/ms365backupd/token.json` (mode 0600).

- The access token is renewed automatically whenever it is needed. Entra ID rotates the
  refresh token on use, and every new one is saved immediately.
- The refresh token stays valid as long as it is used at least every ~90 days (sliding
  window), so a daily backup never needs a new sign-in on its own.
- A new sign-in is needed only when the refresh token is revoked: password change or reset,
  "revoke sessions" by an admin, Conditional Access / sign-in frequency policies, or the app
  registration being changed. The daemon then sends a failure mail right away (no retries),
  and the TUI shows the problem. Press l (or run `ms365backupd login`); the running daemon
  uses the new token on its next run, no restart needed.
- Temporary errors while renewing (network down, Entra ID 5xx) are retried and do **not**
  count as "login required".

Example `config.json`:

```json
{
  "tenant_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
  "client_id": "11111111-2222-3333-4444-555555555555",
  "target": "/mnt/backup",
  "require_mount": true,
  "exclude": [
    "onedrive/Videos",
    "onedrive/Pictures/Camera Roll",
    "**/node_modules",
    "**/*.iso",
    "mail/Junk Email",
    "mail/Deleted Items"
  ],
  "services": { "onedrive": true, "mail": true, "contacts": true, "calendar": true },
  "sharepoint_sites": ["*", "contoso.sharepoint.com:/sites/Archive"],
  "schedule": "02:30",
  "retry_attempts": 3,
  "retry_delay": "20m",
  "poll_interval": "30s",
  "concurrency": 4,
  "keep_deleted": true,
  "min_free_mb": 1024,
  "log_level": "info",
  "email": {
    "to": ["admin@example.com"],
    "via": "smtp",
    "from": "backup@example.com",
    "smtp_host": "smtp.example.com",
    "smtp_port": 587,
    "username": "backup@example.com",
    "password_file": "~/.config/ms365backupd/smtp-password",
    "tls": "starttls",
    "weekly_report_day": "monday",
    "weekly_report_time": "08:00"
  }
}
```

**Exclude patterns** are matched case-insensitively against paths relative to
`<target>/ms365backupd`, and every parent path is checked, so `onedrive/Videos` excludes the whole
folder. `**` and `*` globs are supported. A whole service can be excluded by its name, for
example `mail`. Changing the list triggers a full re-check on the next run: newly included
folders are fetched. Copies of newly excluded folders that are already on the disk are kept
but no longer updated; delete them by hand if you want the space back.

**SharePoint**: `sharepoint_sites` defaults to `["*"]`. `*` backs up the tenant's root site
plus the site of every Microsoft 365 group (and therefore every Team) the account is a
member of, with all their document libraries. Sites that do not belong to a group (classic
team sites, communication sites) cannot be discovered with delegated `Files.Read.All`; add
them explicitly as `host:/sites/Name` (a full URL works too; just `host` is the root site).
Entries can be combined; a site found twice is backed up once. Exclude a single site with
`sharepoint/<Site name>`, or set `"sharepoint_sites": []` to disable SharePoint. A missing or
`null` value means `["*"]`. Sites are stored as `sharepoint/<site display name>/<library>/…`.

**Email:** `via: "smtp"` uses any SMTP server. Credentials are only sent over TLS. `via: "graph"`
sends through the backed-up mailbox and needs the `Mail.Send` permission. If no `email.to`
is set, no mail is sent. Set `weekly_report_day` to `"off"` to disable the weekly report.

**Additional mailboxes** (`mailboxes`) are backed up to `mailboxes/<name>/`, independently
of `services.mail`. Manage them with `ms365backupd mailbox …` (writes the config file right away) or in the
setup's Mailboxes step (press **a** on the dashboard; written when you save the setup). Both
tell a running daemon. Each entry is one object:

```json
"mailboxes": [
  { "name": "shared", "type": "graph" },
  { "name": "other-tenant", "type": "graph", "tenant_id": "…", "client_id": "…" },
  { "name": "gmx", "type": "imap", "host": "imap.gmx.net", "username": "me@gmx.de",
    "password_file": "~/.config/ms365backupd/gmx-password" }
]
```

- `type: "graph"` is any Microsoft 365 account with its **own sign-in**
  (`ms365backupd mailbox login <name>`, or **l** in the setup's Mailboxes step). It only asks
  for `Mail.Read`. `tenant_id`/`client_id` default to the main ones; for an account in another
  tenant, use an app registration that allows that tenant (e.g. multitenant, or one registered
  there). The token is stored in `~/.config/ms365backupd/mailboxes/<name>.token.json`.
- `type: "imap"` takes `host`, `username`, `password` or `password_file` (preferred: keeps
  the password out of the config), `port` (default 993, or 143 with `starttls`/`none`) and
  `tls` (`tls` default, `starttls`, `none`). Messages are tracked by UID; when the server
  changes a folder's UIDVALIDITY, that folder is fetched again.
- `"disabled": true` skips a mailbox but keeps its settings and sign-in.
- A failing mailbox (e.g. sign-in expired) makes the run *partial*, names the mailbox, and does
  not stop the other mailboxes or mark the main account as logged out.
- Removing a mailbox keeps its files on the disk. Exclude single folders with
  `mailboxes/<name>/<folder>`.

**Flags** override the file, for example
`ms365backupd backup --target /Volumes/SSD --require-mount=false -x 'onedrive/Big Stuff'`.
`--exclude` adds to the configured list. `--email-to` and `--services` replace it.

## 4. Run as a service

```sh
ms365backupd service install      # Pi: prints the sudo/systemctl steps; macOS: writes a LaunchAgent
journalctl -u ms365backupd -f     # Pi logs
```

Changes to the config file are applied without a restart: the TUI setup tells the daemon
automatically, otherwise run `sudo systemctl reload ms365backupd` (or `kill -HUP <pid>`). A
running backup finishes with the old settings first. Changing `state_dir` needs a restart.

The daemon:

- checks the disk every `poll_interval`. Connecting a disk starts a backup, unless that disk
  already had a successful backup in the last 20 hours.
- runs the daily backup at `schedule`. A failed run is retried `retry_attempts` times, waiting
  `retry_delay` × attempt between tries. Only then is a failure email sent, at most one per day.
  A missing disk or an expired login is reported right away, because a retry would not help.
- sends the weekly report on `weekly_report_day` at `weekly_report_time`. If the Pi was off at
  that time, the report is sent as soon as it is back up.
- uses a disk-safe write path: download to `*.ms365partial`, verify size and QuickXorHash,
  fsync, then rename. Interrupted downloads resume, and stalled transfers are aborted.
  Graph throttling (429 with Retry-After) is honoured.
- never writes when the disk is unmounted (`require_mount`). It stops the run and reports a
  failure when free space drops below `min_free_mb`.

## Day-to-day commands

```
ms365backupd status            # daemon, disk, next run, last result
ms365backupd tui               # dashboard: b=backup f=full resync c=cancel m=mail report s=setup l=sign in a=mailboxes q=quit
ms365backupd backup [--full] [-s mail,sharepoint,mailboxes]
ms365backupd mailbox list|add|remove|login|logout|test|enable|disable   # additional mailboxes
ms365backupd history -n 30
ms365backupd report --print    # preview the weekly report
ms365backupd completion zsh|bash|fish   # or: make install-completions
```

Examples for additional mailboxes:

```sh
ms365backupd mailbox add shared --type graph --login      # sign in as the other account
ms365backupd mailbox add gmx --type imap --host imap.gmx.net --username me@gmx.de \
    --password-file ~/.config/ms365backupd/gmx-password   # without a password option: prompt
ms365backupd mailbox test gmx
ms365backupd mailbox remove gmx                           # files on the disk are kept
```

In the TUI, **a** opens the setup at the Mailboxes step: **a** add, **d** remove,
**space** enable/disable, **l** sign in (graph), **t** test. Save the setup (ctrl+s) to apply;
esc discards the changes.

`backup` asks the running daemon to start a run. Without a daemon it runs in the foreground.
Exit codes: 0 means success, 2 means partial (some items will be retried), 1 means failure.

## Setup hints and troubleshooting

Things that were not obvious during a real setup:

- **Use the Directory (tenant) ID**, not `common` and not the client ID. `--tenant-id`,
  the `tenant_id` key and the TUI's *Tenant ID* field all take the GUID from the app
  registration's *Overview* page (the primary domain `contoso.onmicrosoft.com` works too).
- **"Allow public client flows" must be Yes** (app registration → *Authentication*).
  Otherwise the token request fails after you entered the code.
- **Missing permissions**: `login` prints "these permissions were not granted: …". Add them
  under *API permissions*, grant consent if needed, and sign in again (l in the TUI).
- **The TUI showed "not logged in" right after a successful sign-in** in older builds; the
  daemon now clears that as soon as the new token is saved. The run that failed before the
  sign-in stays in the history as failed: press **b** to start a backup now.
- **SharePoint empty?** Older configs had `"sharepoint_sites": null`, which meant "none".
  It now means `*`. Sites that are not group/Teams sites must be listed explicitly. The
  log line `sharepoint sites discovered` shows what `*` found.
- **macOS volume names contain spaces**: use `"/Volumes/Extreme SSD"` in JSON, and quote it on
  the command line (`--target "/Volumes/Extreme SSD"`).
- **A different (or reformatted) disk gets a full backup.** This is intended; the sync state is
  tracked per disk.
- **After replacing the binary, restart the daemon** (`sudo systemctl restart ms365backupd`, or
  `launchctl kickstart -k gui/$(id -u)/net.ulbrich.ms365backupd` on macOS). Config changes only
  need a reload; a new binary needs a restart.
- **Additional Graph mailbox "not signed in"**: run `ms365backupd mailbox login <name>` and
  sign in with *that* account (use a private browser window if the browser is already signed
  in to the main account). An account from another tenant needs an app registration that
  accepts it, set as the mailbox's `tenant_id`/`client_id`.
- **IMAP providers with app passwords** (e.g. Gmail, iCloud): use an app
  password; the normal password is rejected.
- **Email via Graph** needs `Mail.Send` on the app *and* a new sign-in after adding it.
  Check with `ms365backupd test-email`.

## Restoring

The files are plain files:

- OneDrive and SharePoint: copy them back, or upload them through the web UI.
- Mail: `.eml` files open in Outlook, Apple Mail or Thunderbird, and can be dragged back into a mailbox.
  `ImportExportTools NG` for Thunderbird can bulk-import a folder.
- Contacts: the `.vcf` files import into Outlook, iCloud and Google.
- Calendar: the events are stored as the full Graph JSON objects.

## Limitations

- Not backed up: Teams chats, OneNote notebooks as live notebooks (their files under OneDrive
  are backed up), Planner, the online archive mailbox, and attachments of calendar events.
- Calendars are stored as JSON, not ICS.
- Contacts and calendars are small, so they are re-read completely on every run. Mail and files are delta-based.
- OneDrive's server-side version history is not backed up. Instead, when a file changes,
  the previously backed-up copy is moved to `.deleted/<date>/` (with `keep_deleted`). Prune
  that folder from time to time, for example with
  `find /mnt/backup/ms365backupd/.deleted -mindepth 1 -maxdepth 1 -mtime +180 -exec rm -rf {} +`.

## Development

```sh
make           # list all targets
make setup     # install Go, xz, Docker and Raspberry Pi Imager (Homebrew or apt)
make test      # unit tests + an end-to-end test against a fake Graph server
make check     # gofmt check, vet and tests, as CI runs them
make dist      # all release binaries + SHA256SUMS in bin/
```

Work happens on `develop`; `main` holds what is released. To release, merge `develop` into
`main`, push, and run `make release` on `main`. It tags the next `vx.y.z` (patch + 1, or
`VERSION=1.2.3 make release`), pushes the tag and waits until GitHub Actions has published the
release with the binaries. `make next-version` shows what the next tag would be.
