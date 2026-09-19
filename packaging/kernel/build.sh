#!/bin/sh
# Builds one guest kernel inside the pinned image. Called by `make kernel ARCH=<arch>`, never by hand.
set -eu

ARCH="${1:?arch: arm64 or amd64}"
KERNEL_VERSION="${KERNEL_VERSION:?}"
KERNEL_SHA256="${KERNEL_SHA256:?}"
SRC=/src
OUT=/out

case "$ARCH" in
arm64) KARCH=arm64; CROSS=aarch64-linux-gnu-; TARGET=Image; ARTIFACT=arch/arm64/boot/Image; OUTNAME=Image-arm64 ;;
amd64) KARCH=x86_64; CROSS=; TARGET=vmlinux; ARTIFACT=vmlinux; OUTNAME=vmlinux-amd64 ;;
*) echo "unknown arch $ARCH" >&2; exit 2 ;;
esac

tarball="/cache/linux-$KERNEL_VERSION.tar.xz"
mkdir -p /cache "$OUT"
if [ ! -f "$tarball" ]; then
	curl -fsSL -o "$tarball.part" "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$KERNEL_VERSION.tar.xz"
	mv "$tarball.part" "$tarball"
fi
echo "$KERNEL_SHA256  $tarball" | sha256sum -c - >/dev/null

rm -rf "$SRC"
mkdir -p "$SRC"
tar -xJf "$tarball" -C "$SRC" --strip-components=1
cp "/config/config-$ARCH" "$SRC/.config"

# A fixed timestamp, user, host and version are what make two builds byte-identical.
export KBUILD_BUILD_TIMESTAMP='Thu Jan  1 00:00:00 UTC 1970'
export KBUILD_BUILD_USER=shard
export KBUILD_BUILD_HOST=shard
export KBUILD_BUILD_VERSION=1
export SOURCE_DATE_EPOCH=0

cd "$SRC"
make ARCH="$KARCH" CROSS_COMPILE="$CROSS" olddefconfig >/dev/null
make ARCH="$KARCH" CROSS_COMPILE="$CROSS" -j"$(nproc)" "$TARGET" >/dev/null

cp "$ARTIFACT" "$OUT/$OUTNAME"
(cd "$OUT" && sha256sum "$OUTNAME" > "$OUTNAME.sha256")
cat "$OUT/$OUTNAME.sha256"
