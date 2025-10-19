#!/bin/sh
set -eu

mkdir -p /usr/local/etc/pkg/repos
cat > /usr/local/etc/pkg/repos/FreeBSD.conf <<'EOF'
FreeBSD: {
  url: "pkg+https://pkg.FreeBSD.org/${ABI}/latest"
}
FreeBSD-base: {
  url: "pkg+https://pkg.FreeBSD.org/${ABI}/base_release_${VERSION_MINOR}",
  mirror_type: "srv",
  signature_type: "fingerprints",
  fingerprints: "/usr/share/keys/pkg",
  enabled: yes
}
FreeBSD-kmods: {
  enabled: no
}
EOF

echo "::group::Setting up FreeBSD VM"

sysctl net.inet.ip.forwarding=1
pkg install -y FreeBSD-pf FreeBSD-zfs podman-suite

truncate -s 16G /var/tmp/z
mkdir -p /var/db/containers/storage
zpool create -R /var/db/containers/storage -O mountpoint=/ -O compression=lz4 z /var/tmp/z

echo "::endgroup::"
