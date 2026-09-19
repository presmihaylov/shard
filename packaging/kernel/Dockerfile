# The one toolchain every guest kernel is built with; the digest is the reproducibility pin.
FROM --platform=linux/amd64 debian:13@sha256:9cc080028c43b27d2074d63a5f9caf7166d731494965616c1a6d2827a004585c

RUN apt-get update && apt-get install -y --no-install-recommends \
    bc bison build-essential ca-certificates cpio curl flex gcc-aarch64-linux-gnu \
    kmod libelf-dev libssl-dev python3 xz-utils \
    && rm -rf /var/lib/apt/lists/*

COPY build.sh /build.sh
ENTRYPOINT ["/build.sh"]
