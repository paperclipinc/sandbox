#!/usr/bin/env bash
set -euo pipefail

# Builds a minimal rootfs ext4 image with the guest agent baked in.
# Must run on Linux as root (needs mount, chroot, debootstrap).
#
# Usage:
#   ./guest/rootfs/build.sh [output_path] [size_mb]
#
# Produces: rootfs.ext4 with:
#   /init              → guest agent binary (PID 1)
#   /bin/sh            → busybox or bash
#   /usr/bin/python3   → Python 3 (optional, for python template)
#   /workspace/        → agent working directory

OUTPUT="${1:-/tmp/mitos-rootfs.ext4}"
SIZE_MB="${2:-512}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORK_DIR=$(mktemp -d)
MOUNT_DIR="${WORK_DIR}/mnt"

cleanup() {
    umount "$MOUNT_DIR" 2>/dev/null || true
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

echo "==> Building guest agent (static binary)"
cd "$PROJECT_ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "${WORK_DIR}/agent" ./guest/agent/

echo "==> Creating ext4 image (${SIZE_MB}MB)"
dd if=/dev/zero of="$OUTPUT" bs=1M count="$SIZE_MB" status=none
mkfs.ext4 -q -F "$OUTPUT"

echo "==> Mounting and populating rootfs"
mkdir -p "$MOUNT_DIR"
mount -o loop "$OUTPUT" "$MOUNT_DIR"

# Create directory structure
mkdir -p "$MOUNT_DIR"/{bin,sbin,usr/bin,usr/lib,lib,lib64,dev,proc,sys,tmp,run,etc,workspace,var/log}

# Install the guest agent as /init
cp "${WORK_DIR}/agent" "$MOUNT_DIR/init"
chmod +x "$MOUNT_DIR/init"

# Check if we should use debootstrap for a full Ubuntu rootfs or minimal busybox
if command -v debootstrap &>/dev/null && [ "${FULL_ROOTFS:-0}" = "1" ]; then
    echo "==> Installing Ubuntu minimal via debootstrap"
    debootstrap --variant=minbase noble "$MOUNT_DIR" http://archive.ubuntu.com/ubuntu

    # Install Python
    chroot "$MOUNT_DIR" apt-get update -qq
    chroot "$MOUNT_DIR" apt-get install -y --no-install-recommends \
        python3 python3-pip python3-venv ca-certificates curl
    chroot "$MOUNT_DIR" apt-get clean
    chroot "$MOUNT_DIR" rm -rf /var/lib/apt/lists/*

    # Symlink init
    rm -f "$MOUNT_DIR/sbin/init"
    ln -s /init "$MOUNT_DIR/sbin/init"
else
    echo "==> Installing busybox (minimal rootfs)"
    # Download static busybox
    BUSYBOX_URL="https://busybox.net/downloads/binaries/1.35.0-x86_64-linux-musl/busybox"
    if command -v curl &>/dev/null; then
        curl -fsSL -o "$MOUNT_DIR/bin/busybox" "$BUSYBOX_URL"
    elif command -v wget &>/dev/null; then
        wget -q -O "$MOUNT_DIR/bin/busybox" "$BUSYBOX_URL"
    fi
    chmod +x "$MOUNT_DIR/bin/busybox"

    # Create symlinks for common commands
    for cmd in sh ash ls cat echo mkdir rm cp mv chmod chown ln env wc head tail grep sed awk sort uniq tr tee; do
        ln -sf /bin/busybox "$MOUNT_DIR/bin/$cmd"
    done
    for cmd in python3 pip3; do
        # Stub: real Python needs debootstrap or a pre-built rootfs
        cat > "$MOUNT_DIR/usr/bin/$cmd" << 'PYSTUB'
#!/bin/sh
echo "Python not available in minimal rootfs. Use FULL_ROOTFS=1 to build with Python."
exit 1
PYSTUB
        chmod +x "$MOUNT_DIR/usr/bin/$cmd"
    done
fi

# Basic config files
echo "sandbox" > "$MOUNT_DIR/etc/hostname"
echo "root:x:0:0:root:/root:/bin/sh" > "$MOUNT_DIR/etc/passwd"
echo "root:x:0:" > "$MOUNT_DIR/etc/group"
echo "nameserver 8.8.8.8" > "$MOUNT_DIR/etc/resolv.conf"

# Create /etc/os-release
cat > "$MOUNT_DIR/etc/os-release" << 'EOF'
NAME="sandbox"
ID=sandbox
VERSION="1.0"
PRETTY_NAME="sandbox rootfs"
EOF

echo "==> Unmounting"
umount "$MOUNT_DIR"

# Report
SIZE=$(du -sh "$OUTPUT" | cut -f1)
echo ""
echo "================================"
echo "  Rootfs built: $OUTPUT ($SIZE)"
echo "================================"
