#!/usr/bin/env bash
# Builds a headless Raspberry Pi OS Lite (64-bit) image with ms365backupd and a set of
# CLI tools preinstalled (see script/pi-image/provision.sh). Only the ms365backupd
# configuration is missing. The build runs in a privileged Docker container, so it works
# on Linux and macOS (Docker Desktop); on x86_64 hosts arm64 emulation is set up first.
#
# Usage: make image [PI_USER=jan] [PI_PASSWORD=…] [PI_SSH_KEY=~/.ssh/id_ed25519.pub] …
# All settings are environment variables; see the defaults below and README.md. They can
# also be kept in pi-image/env (PI_IMAGE_ENV=… for another file).
set -euo pipefail

cd "$(dirname "$0")/.."

# Settings file (shell syntax, KEY=value). Variables already set in the environment, e.g.
# by make image PI_USER=jan, take precedence over the file.
settings=(PI_IMAGE_URL PI_HOSTNAME MDNS_HOSTNAME PI_USER PI_PASSWORD PI_SSH_KEY PI_TIMEZONE
	PI_SSH_TRUSTED_NETS WIFI_SSID WIFI_PASSWORD WIFI_COUNTRY TS_AUTHKEY NODE_MAJOR IMAGE_UPGRADE IMAGE_COMPRESS IMAGE_NAME)
env_file="${PI_IMAGE_ENV:-pi-image/env}"
if [[ -f "$env_file" ]]; then
	saved=""
	for v in "${settings[@]}"; do
		# declare -p fails for unset variables ([[ -v ]] needs bash 4.2; macOS has 3.2)
		if declare -p "$v" >/dev/null 2>&1; then saved+="$(declare -p "$v")"$'\n'; fi
	done
	set -a
	# shellcheck source=/dev/null
	. "$env_file"
	set +a
	eval "$saved"
	echo "pi-image: settings from $env_file"
elif [[ -n "${PI_IMAGE_ENV:-}" ]]; then
	echo "pi-image: PI_IMAGE_ENV not found: $PI_IMAGE_ENV" >&2
	exit 1
fi

export APP_VSN="${APP_VSN:-dev}"
export PI_IMAGE_URL="${PI_IMAGE_URL:-https://downloads.raspberrypi.com/raspios_lite_arm64_latest}"
export PI_HOSTNAME="${PI_HOSTNAME:-ms365backup}"
export MDNS_HOSTNAME="${MDNS_HOSTNAME:-}"
PI_SSH_TRUSTED_NETS="${PI_SSH_TRUSTED_NETS:-}"
export PI_SSH_TRUSTED_NETS="${PI_SSH_TRUSTED_NETS// /}"
export PI_USER="${PI_USER:-pi}"
PI_PASSWORD="${PI_PASSWORD:-}"
PI_SSH_KEY="${PI_SSH_KEY:-}"
export PI_TIMEZONE="${PI_TIMEZONE:-}"
export WIFI_SSID="${WIFI_SSID:-}" WIFI_PASSWORD="${WIFI_PASSWORD:-}" WIFI_COUNTRY="${WIFI_COUNTRY:-}"
export TS_AUTHKEY="${TS_AUTHKEY:-}"
export NODE_MAJOR="${NODE_MAJOR:-26}"
export IMAGE_UPGRADE="${IMAGE_UPGRADE:-1}"
export IMAGE_COMPRESS="${IMAGE_COMPRESS:-1}"
export IMAGE_NAME="${IMAGE_NAME:-ms365backupd-rpi-arm64-$APP_VSN}"

die() { echo "pi-image: $*" >&2; exit 1; }

[[ -f bin/ms365backupd-linux-arm64 ]] || die "bin/ms365backupd-linux-arm64 missing (run make build-pi)"
[[ "$PI_USER" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die "invalid PI_USER: $PI_USER"
hostname_re='^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$'
[[ "$PI_HOSTNAME" =~ $hostname_re ]] || die "invalid PI_HOSTNAME: $PI_HOSTNAME"
[[ -z "$MDNS_HOSTNAME" || "$MDNS_HOSTNAME" =~ $hostname_re ]] ||
	die "invalid MDNS_HOSTNAME: $MDNS_HOSTNAME (a name like foo, without .local)"
if [[ -n "$PI_SSH_TRUSTED_NETS" ]]; then
	IFS=, read -r -a nets <<<"$PI_SSH_TRUSTED_NETS"
	for n in "${nets[@]}"; do
		[[ "$n" =~ ^[0-9A-Fa-f:.]+(/[0-9]{1,3})?$ ]] ||
			die "invalid PI_SSH_TRUSTED_NETS entry: '$n' (comma-separated addresses or CIDR networks)"
	done
fi
[[ "$NODE_MAJOR" =~ ^[0-9]+$ ]] || die "invalid NODE_MAJOR: $NODE_MAJOR"
if [[ -n "$WIFI_SSID" && ! "$WIFI_COUNTRY" =~ ^[A-Z]{2}$ ]]; then
	die "WIFI_SSID needs WIFI_COUNTRY (two-letter code such as DE or US)"
fi

# Default SSH keys: pi-image/ssh/authorized_keys in the repo, else the first public key in ~/.ssh.
if [[ -z "$PI_SSH_KEY" ]]; then
	for k in pi-image/ssh/authorized_keys ~/.ssh/id_ed25519.pub ~/.ssh/id_ecdsa.pub ~/.ssh/id_rsa.pub; do
		[[ -f "$k" ]] && { PI_SSH_KEY="$k"; break; }
	done
fi
export PI_SSH_PUBKEY=""
if [[ -n "$PI_SSH_KEY" ]]; then
	[[ -f "$PI_SSH_KEY" ]] || die "PI_SSH_KEY not found: $PI_SSH_KEY"
	PI_SSH_PUBKEY="$(grep -E '^(ssh-|ecdsa-|sk-)' "$PI_SSH_KEY")" || die "no public key in $PI_SSH_KEY"
fi
# Default password: generated and printed (without ambiguous characters such as 0/O, 1/l).
generated_password=""
if [[ -z "$PI_PASSWORD" ]]; then
	PI_PASSWORD="$(LC_ALL=C tr -dc 'A-HJ-NP-Za-km-z2-9' </dev/urandom | head -c 16 || true)"
	[[ ${#PI_PASSWORD} -eq 16 ]] || die "could not generate a password"
	generated_password=1
fi
export PI_PASSWORD

# Default time zone: the host's.
if [[ -z "$PI_TIMEZONE" ]]; then
	PI_TIMEZONE="$(readlink /etc/localtime 2>/dev/null | sed -n 's#.*/zoneinfo/##p' || true)"
	PI_TIMEZONE="${PI_TIMEZONE:-Etc/UTC}"
fi

command -v docker >/dev/null || die "docker is required"
arch="$(docker info --format '{{.Architecture}}')"
case "$arch" in
aarch64 | arm64) ;;
*)
	if ! docker run --rm --privileged debian:trixie test -e /proc/sys/fs/binfmt_misc/qemu-aarch64; then
		echo "pi-image: registering arm64 emulation (tonistiigi/binfmt) on the Docker host"
		docker run --rm --privileged tonistiigi/binfmt --install arm64
	fi
	;;
esac

mkdir -p .cache/pi-image bin
echo "pi-image: building bin/$IMAGE_NAME.img (user $PI_USER, host $PI_HOSTNAME, ${MDNS_HOSTNAME:-$PI_HOSTNAME}.local, tz $PI_TIMEZONE," \
	"ssh keys: ${PI_SSH_KEY:-none} ($(grep -c . <<<"$PI_SSH_PUBKEY" || true)))"
print_password() {
	[[ -n "$generated_password" ]] || return 0
	echo
	echo "pi-image: generated password for $PI_USER: $PI_PASSWORD"
	echo
}
print_password

# Values are passed by name (-e VAR) so secrets do not show up in the process list.
docker run --rm --privileged \
	-e APP_VSN -e PI_IMAGE_URL -e PI_HOSTNAME -e MDNS_HOSTNAME -e PI_USER -e PI_PASSWORD -e PI_SSH_PUBKEY \
	-e PI_SSH_TRUSTED_NETS \
	-e PI_TIMEZONE -e WIFI_SSID -e WIFI_PASSWORD -e WIFI_COUNTRY -e TS_AUTHKEY -e NODE_MAJOR \
	-e IMAGE_UPGRADE -e IMAGE_COMPRESS -e IMAGE_NAME \
	-v "$PWD:/src:ro" -v "$PWD/.cache/pi-image:/cache" -v "$PWD/bin:/out" \
	debian:trixie bash /src/script/pi-image/build.sh
print_password
