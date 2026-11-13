#!/usr/bin/env bash
# Installs the tools to build ms365backupd and to build and flash the Raspberry Pi image:
# Go, xz, Docker and Raspberry Pi Imager. Tools that are already there are skipped.
# macOS uses Homebrew; Debian/Ubuntu/Raspberry Pi OS use apt.
set -euo pipefail

have() { command -v "$1" >/dev/null 2>&1; }
ok() { printf '  %-22s %s\n' "$1" "${2:-already installed}"; }

case "$(uname -s)" in
Darwin)
	have brew || {
		echo "Homebrew is required: https://brew.sh" >&2
		exit 1
	}
	if have go; then ok go "$(go version | cut -d' ' -f3)"; else brew install go; fi
	if have xzcat; then ok xz; else brew install xz; fi
	if have docker || [[ -d /Applications/Docker.app ]]; then ok docker; else brew install --cask docker-desktop; fi
	if [[ -d "/Applications/Raspberry Pi Imager.app" ]]; then ok "Raspberry Pi Imager"; else brew install --cask raspberry-pi-imager; fi
	have docker && docker info >/dev/null 2>&1 || echo "Start Docker Desktop once before running make image."
	;;
Linux)
	have apt-get || {
		echo "Install Go, xz, Docker and Raspberry Pi Imager with your package manager." >&2
		exit 1
	}
	pkgs=()
	have xzcat || pkgs+=(xz-utils)
	have docker || pkgs+=(docker.io)
	have rpi-imager || { apt-cache show rpi-imager >/dev/null 2>&1 && pkgs+=(rpi-imager); }
	if ((${#pkgs[@]})); then
		sudo apt-get update
		sudo apt-get install -y "${pkgs[@]}"
	fi
	ok "xz, docker, rpi-imager" "checked"
	want=$(sed -n 's/^go \([0-9.]*\).*/\1/p' "$(dirname "$0")/../go.mod")
	if have go; then
		ok go "$(go version | cut -d' ' -f3) (go.mod needs $want)"
	else
		echo "Go $want or newer is needed; Debian's golang package is usually older: https://go.dev/dl/"
	fi
	have rpi-imager || echo "Raspberry Pi Imager: https://www.raspberrypi.com/software/ (or flash with xzcat | dd, see README)"
	;;
*)
	echo "unsupported OS: $(uname -s)" >&2
	exit 1
	;;
esac
