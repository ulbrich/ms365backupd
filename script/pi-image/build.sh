#!/usr/bin/env bash
# Runs inside the privileged debian:trixie container started by script/pi-image.sh:
# downloads Raspberry Pi OS Lite, grows it, provisions it in an arm64 chroot, shrinks it
# and writes /out/$IMAGE_NAME.img(.xz).
set -euo pipefail

log() { echo "==> $*"; }
export DEBIAN_FRONTEND=noninteractive

log "installing build tools"
apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl xz-utils fdisk e2fsprogs \
	dosfstools zerofree openssl >/dev/null

# Containers usually lack loop device nodes.
[[ -e /dev/loop-control ]] || mknod /dev/loop-control c 10 237
for i in $(seq 0 63); do [[ -e /dev/loop$i ]] || mknod /dev/loop$i b 7 "$i"; done

work=/work
img=$work/pi.img
root=$work/root
mkdir -p "$work" "$root"
loops=()

cleanup() {
	set +e
	for m in boot/firmware dev/pts dev sys proc ""; do
		mountpoint -q "$root/$m" && umount -l "$root/$m"
	done
	for l in "${loops[@]}"; do losetup -d "$l" 2>/dev/null; done
}
trap cleanup EXIT

# part <n> start|size: partition geometry in 512-byte sectors.
part() { sfdisk -d "$img" | sed -n "s#^$img$1 : start= *\([0-9]*\), size= *\([0-9]*\).*#\1 \2#p" | cut -d' ' -f"$([[ $2 == start ]] && echo 1 || echo 2)"; }
loop() { # loop <offset-sectors> [size-sectors]: sets $dev (no subshell, so cleanup sees it)
	dev=$(losetup -f --show -o $(($1 * 512)) ${2:+--sizelimit $(($2 * 512))} "$img")
	loops+=("$dev")
}

# --- download ---------------------------------------------------------------------
url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$PI_IMAGE_URL")
file=/cache/$(basename "$url")
if [[ ! -f "$file" ]]; then
	log "downloading $url"
	curl -fSL --retry 3 -o "$file.part" "$url"
	mv "$file.part" "$file"
fi
if curl -fsSL -o "$file.sha256" "$url.sha256"; then
	(cd /cache && sha256sum -c "$(basename "$file").sha256")
else
	echo "warning: no checksum published for $url" >&2
fi
log "decompressing $(basename "$file")"
xz -dc "$file" >"$img"

# --- grow the root partition so everything fits ----------------------------------
truncate -s +6G "$img"
echo ", +" | sfdisk -q -N 2 --no-reread "$img"
loop "$(part 2 start)"
rootdev=$dev
e2fsck -fy "$rootdev" >/dev/null || [[ $? -le 1 ]]
resize2fs -p "$rootdev" >/dev/null

# --- mount and provision ----------------------------------------------------------
mount "$rootdev" "$root"
mkdir -p "$root/boot/firmware"
loop "$(part 1 start)" "$(part 1 size)"
mount -t vfat "$dev" "$root/boot/firmware"
mount -t proc proc "$root/proc"
mount -t sysfs sys "$root/sys"
mount --bind /dev "$root/dev"
mount --bind /dev/pts "$root/dev/pts"

cp --remove-destination /etc/resolv.conf "$root/etc/resolv.conf.build"
mv "$root/etc/resolv.conf" "$root/etc/resolv.conf.orig" 2>/dev/null || true
mv "$root/etc/resolv.conf.build" "$root/etc/resolv.conf"

install -m755 /src/bin/ms365backupd-linux-arm64 "$root/usr/local/bin/ms365backupd"
install -d "$root/usr/local/share/doc/ms365backupd"
install -m644 /src/README.md /src/deploy/systemd/fstab.example "$root/usr/local/share/doc/ms365backupd/"
install -m755 /src/script/pi-image/provision.sh "$root/root/provision.sh"

PI_PASSWORD_HASH=""
[[ -n "$PI_PASSWORD" ]] && PI_PASSWORD_HASH=$(openssl passwd -6 -stdin <<<"$PI_PASSWORD")
log "provisioning (arm64 chroot)"
chroot "$root" /usr/bin/env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
	HOME=/root TERM=dumb LANG=C.UTF-8 \
	APP_VSN="$APP_VSN" PI_HOSTNAME="$PI_HOSTNAME" MDNS_HOSTNAME="$MDNS_HOSTNAME" PI_SSH_TRUSTED_NETS="$PI_SSH_TRUSTED_NETS" PI_USER="$PI_USER" PI_PASSWORD_HASH="$PI_PASSWORD_HASH" \
	PI_SSH_PUBKEY="$PI_SSH_PUBKEY" TS_AUTHKEY="$TS_AUTHKEY" NODE_MAJOR="$NODE_MAJOR" \
	IMAGE_UPGRADE="$IMAGE_UPGRADE" /bin/bash /root/provision.sh
rm -f "$root/root/provision.sh"
rm -f "$root/etc/resolv.conf"
mv "$root/etc/resolv.conf.orig" "$root/etc/resolv.conf" 2>/dev/null || true

# --- first-boot settings for cloud-init (boot partition) -------------------------
yq() { local s=${1//\\/\\\\}; s=${s//\"/\\\"}; printf '"%s"' "$s"; } # YAML double-quoted string
pwauth=false
[[ -z "$PI_SSH_PUBKEY" ]] && pwauth=true
cat >"$root/boot/firmware/user-data" <<EOF
#cloud-config
# Written by ms365backupd's make image. The user ($PI_USER) was created in the image,
# so cloud-init must not create or lock one.
hostname: $PI_HOSTNAME
manage_etc_hosts: true
users: []
timezone: $PI_TIMEZONE
ssh_pwauth: $pwauth
EOF
if [[ -n "$WIFI_SSID" ]]; then
	cat >"$root/boot/firmware/network-config" <<EOF
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
      optional: true
  wifis:
    wlan0:
      dhcp4: true
      optional: true
      regulatory-domain: $WIFI_COUNTRY
      access-points:
        $(yq "$WIFI_SSID"):
          password: $(yq "$WIFI_PASSWORD")
EOF
fi

for m in boot/firmware dev/pts dev sys proc ""; do umount "$root/$m"; done

# --- shrink: minimal filesystem plus headroom; the first boot expands it ----------
log "shrinking"
e2fsck -fy "$rootdev" >/dev/null || [[ $? -le 1 ]]
resize2fs -M "$rootdev" >/dev/null
blocks=$(dumpe2fs -h "$rootdev" 2>/dev/null | sed -n 's/^Block count: *//p')
bsize=$(dumpe2fs -h "$rootdev" 2>/dev/null | sed -n 's/^Block size: *//p')
fs_sectors=$((blocks * bsize / 512))
new_sectors=$((fs_sectors + 512 * 1024 * 2)) # +512 MiB
resize2fs "$rootdev" $((new_sectors / 2))K >/dev/null
zerofree "$rootdev"
losetup -d "$rootdev"
start=$(part 2 start)
echo "$start, $new_sectors" | sfdisk -q -N 2 --no-reread "$img"
truncate -s $(((start + new_sectors) * 512)) "$img"

if [[ "$IMAGE_COMPRESS" == 1 ]]; then
	log "compressing"
	xz -T0 -6 -c "$img" >"/out/$IMAGE_NAME.img.xz"
	out="$IMAGE_NAME.img.xz"
else
	cp --sparse=always "$img" "/out/$IMAGE_NAME.img"
	out="$IMAGE_NAME.img"
fi
(cd /out && sha256sum "$out" >"$out.sha256")
chown "$(stat -c %u:%g /out)" "/out/$out" "/out/$out.sha256"
log "done: bin/$out ($(du -h "/out/$out" | cut -f1))"
