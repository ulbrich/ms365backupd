#!/bin/bash
# Runs inside the Raspberry Pi OS (arm64) chroot, called by script/pi-image/build.sh.
# Installs the tools, sets up the login user and ms365backupd (without a config).
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive

# No service starts inside the chroot.
printf '#!/bin/sh\nexit 101\n' >/usr/sbin/policy-rc.d
chmod 755 /usr/sbin/policy-rc.d

apt-get update
if [[ "$IMAGE_UPGRADE" == 1 ]]; then
	apt-get -y -o Dpkg::Options::=--force-confold full-upgrade
fi
apt-get install -y --no-install-recommends \
	ca-certificates curl gnupg git rsync openssh-server bash-completion less unzip xz-utils \
	ack ripgrep fd-find jq fzf bat tree htop ncdu neovim \
	python3 python3-pip python3-venv \
	pandoc weasyprint fonts-dejavu-core \
	exfatprogs
ln -sf /usr/bin/fdfind /usr/local/bin/fd
ln -sf /usr/bin/batcat /usr/local/bin/bat

# --- third-party apt repositories: GitHub CLI, glow (Charm), Tailscale, Node.js LTS ---
codename=$(. /etc/os-release && echo "$VERSION_CODENAME")
install -d -m755 /etc/apt/keyrings
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli.gpg
echo "deb [signed-by=/etc/apt/keyrings/githubcli.gpg] https://cli.github.com/packages stable main" \
	>/etc/apt/sources.list.d/github-cli.list
curl -fsSL https://repo.charm.sh/apt/gpg.key | gpg --dearmor --yes -o /etc/apt/keyrings/charm.gpg
echo "deb [signed-by=/etc/apt/keyrings/charm.gpg] https://repo.charm.sh/apt/ * *" \
	>/etc/apt/sources.list.d/charm.list
curl -fsSL "https://pkgs.tailscale.com/stable/debian/$codename.noarmor.gpg" \
	-o /usr/share/keyrings/tailscale-archive-keyring.gpg
curl -fsSL "https://pkgs.tailscale.com/stable/debian/$codename.tailscale-keyring.list" \
	-o /etc/apt/sources.list.d/tailscale.list
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor --yes -o /etc/apt/keyrings/nodesource.gpg
echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
	>/etc/apt/sources.list.d/nodesource.list
# Prefer NodeSource over the (older) Debian/Raspberry Pi nodejs packages.
printf 'Package: nodejs\nPin: origin deb.nodesource.com\nPin-Priority: 600\n' >/etc/apt/preferences.d/nodesource
apt-get update
apt-get install -y --no-install-recommends gh glow tailscale nodejs

# --- tools without apt packages (system-wide) -----------------------------------------
npm install -g @earendil-works/pi-coding-agent # pi
npm cache clean --force
curl -fsSL -o /usr/local/bin/herdr https://github.com/herdrdev/herdr/releases/latest/download/herdr-linux-aarch64
chmod 755 /usr/local/bin/herdr
curl -fsSL https://openknowledge.sh/install | OPENKNOWLEDGE_INSTALL_DIR=/usr/local/bin bash # okn
curl -fsSL https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin UV_NO_MODIFY_PATH=1 sh

# --- login user ------------------------------------------------------------------------
# The image ships a locked "pi" (uid 1000) that the first-boot wizard would rename; we
# rename and unlock it here and disable the wizard.
if [[ "$PI_USER" != pi ]] && id pi >/dev/null 2>&1 && ! id "$PI_USER" >/dev/null 2>&1; then
	usermod -l "$PI_USER" -d "/home/$PI_USER" -m pi
	groupmod -n "$PI_USER" pi
fi
id "$PI_USER" >/dev/null 2>&1 || useradd -m -s /bin/bash "$PI_USER"
groups=()
for g in adm dialout cdrom audio users sudo video plugdev input gpio spi i2c netdev render; do
	getent group "$g" >/dev/null && groups+=("$g")
done
usermod -s /bin/bash -aG "$(IFS=,; echo "${groups[*]}")" "$PI_USER"
if [[ -n "$PI_PASSWORD_HASH" ]]; then
	usermod -p "$PI_PASSWORD_HASH" "$PI_USER"
else
	usermod -p '*' "$PI_USER" # no password: SSH key only, sudo needs none
fi
echo "$PI_USER ALL=(ALL) NOPASSWD: ALL" >"/etc/sudoers.d/010_$PI_USER-nopasswd"
chmod 440 "/etc/sudoers.d/010_$PI_USER-nopasswd"
home=$(getent passwd "$PI_USER" | cut -d: -f6)
if [[ -n "$PI_SSH_PUBKEY" ]]; then
	install -d -m700 -o "$PI_USER" -g "$PI_USER" "$home/.ssh"
	printf '%s\n' "$PI_SSH_PUBKEY" >"$home/.ssh/authorized_keys"
	chown "$PI_USER:$PI_USER" "$home/.ssh/authorized_keys"
	chmod 600 "$home/.ssh/authorized_keys"
fi
systemctl disable userconfig.service 2>/dev/null || true
rm -f /etc/ssh/sshd_config.d/rename_user.conf

# Claude Code: native installer, per user (~/.local/bin, auto-updates).
su - "$PI_USER" -c 'curl -fsSL https://claude.ai/install.sh | bash'
su - "$PI_USER" -c 'grep -q "\.local/bin" ~/.bashrc || echo '"'"'export PATH="$HOME/.local/bin:$PATH"'"'"' >>~/.bashrc'

# --- system settings ---------------------------------------------------------------
echo "$PI_HOSTNAME" >/etc/hostname
sed -i "s/^127\.0\.1\.1.*/127.0.1.1\t$PI_HOSTNAME/" /etc/hosts
systemctl set-default multi-user.target

# Bonjour/mDNS: avahi announces <hostname>.local, or MDNS_HOSTNAME.local if set, plus the
# SSH service (shows up in dns-sd -B _ssh._tcp and SSH browsers).
if [[ -n "$MDNS_HOSTNAME" ]]; then
	sed -i "s/^#\?host-name=.*/host-name=$MDNS_HOSTNAME/" /etc/avahi/avahi-daemon.conf
	grep -qx "host-name=$MDNS_HOSTNAME" /etc/avahi/avahi-daemon.conf
fi
cat >/etc/avahi/services/ssh.service <<'EOF'
<?xml version="1.0" standalone='no'?>
<!DOCTYPE service-group SYSTEM "avahi-service.dtd">
<service-group>
  <name replace-wildcards="yes">%h</name>
  <service>
    <type>_ssh._tcp</type>
    <port>22</port>
  </service>
  <service>
    <type>_sftp-ssh._tcp</type>
    <port>22</port>
  </service>
</service-group>
EOF
systemctl enable avahi-daemon
systemctl enable ssh tailscaled

if [[ -n "$TS_AUTHKEY" ]]; then
	install -d -m700 /etc/tailscale
	(umask 077 && printf '%s\n' "$TS_AUTHKEY" >/etc/tailscale/authkey)
	cat >/etc/systemd/system/tailscale-firstboot.service <<'EOF'
[Unit]
Description=Join the tailnet with the auth key from the image (once)
Wants=network-online.target tailscaled.service
After=network-online.target tailscaled.service
ConditionPathExists=/etc/tailscale/authkey

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'tailscale up --auth-key="$(cat /etc/tailscale/authkey)" && rm -f /etc/tailscale/authkey'

[Install]
WantedBy=multi-user.target
EOF
	systemctl enable tailscale-firstboot.service
fi

# --- ms365backupd (binary installed by build.sh; config is created by the user) ----------
ms365backupd completion bash >/usr/share/bash-completion/completions/ms365backupd
install -d -o "$PI_USER" -g "$PI_USER" /mnt/backup
cat >/etc/systemd/system/ms365backupd.service <<EOF
[Unit]
Description=Microsoft 365 backup daemon
Wants=network-online.target
After=network-online.target
# Starts once the configuration exists: sudo systemctl start ms365backupd
ConditionPathExists=$home/.config/ms365backupd/config.json

[Service]
Type=simple
User=$PI_USER
Environment=HOME=$home
ExecStart=/usr/local/bin/ms365backupd run
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=30
Nice=10
IOSchedulingClass=idle
TimeoutStopSec=60
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
systemctl enable ms365backupd.service

cat >/etc/motd <<EOF

ms365backupd $APP_VSN is installed but not configured yet:
  1. Mount the backup disk at /mnt/backup (nofail), see
     /usr/local/share/doc/ms365backupd/fstab.example
  2. ms365backupd                    setup screen; then press l to sign in
  3. sudo systemctl start ms365backupd
  Docs: glow /usr/local/share/doc/ms365backupd/README.md

Tools: claude, pi, herdr, okn, gh, glow, pandoc, node, uv, rg, fd, jq, fzf, bat, nvim
Tailscale: sudo tailscale up

EOF

# --- clean up --------------------------------------------------------------------
apt-get -y autoremove --purge
apt-get clean
rm -rf /var/lib/apt/lists/* /var/cache/apt/*.bin /root/.npm /root/.cache "$home/.cache" /tmp/* /var/tmp/*
# Host keys must be unique per device: cloud-init creates them on first boot; sshd
# creates missing ones itself as a fallback.
rm -f /etc/ssh/ssh_host_*
install -d /etc/systemd/system/ssh.service.d
printf '[Service]\nExecStartPre=/usr/bin/ssh-keygen -A\n' >/etc/systemd/system/ssh.service.d/hostkeys.conf
rm -f /usr/sbin/policy-rc.d
