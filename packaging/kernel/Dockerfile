# The one toolchain every guest kernel is built with; the digest pins the filesystem, the snapshot pins apt.
FROM --platform=linux/amd64 debian:13@sha256:9cc080028c43b27d2074d63a5f9caf7166d731494965616c1a6d2827a004585c

# A dated snapshot never moves, and apt still checks its signed Release file; only its Valid-Until has lapsed.
ARG SNAPSHOT=20260918T000000Z
RUN echo "deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${SNAPSHOT} trixie main" > /etc/apt/sources.list \
    && rm -f /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::Retries=5 update \
    && apt-get -o Acquire::Retries=5 install -y --no-install-recommends \
    bc=1.07.1-4 \
    binutils=2.44-3 \
    binutils-aarch64-linux-gnu=2.44-3 \
    bison=2:3.8.2+dfsg-1+b2 \
    build-essential=12.12 \
    ca-certificates=20250419 \
    cpio=2.15+dfsg-2 \
    curl=8.14.1-2+deb13u5 \
    flex=2.6.4-8.2+b4 \
    gcc-14=14.2.0-19 \
    gcc-14-aarch64-linux-gnu=14.2.0-19cross1 \
    gcc-aarch64-linux-gnu=4:14.2.0-1 \
    kmod=34.2-2 \
    libc6-dev=2.41-12+deb13u4 \
    libelf-dev=0.192-4 \
    libssl-dev=3.5.7-1~deb13u2 \
    make=4.4.1-2 \
    python3=3.13.5-1 \
    xz-utils=5.8.1-1+deb13u1 \
    && rm -rf /var/lib/apt/lists/*

COPY build.sh /build.sh
ENTRYPOINT ["/build.sh"]
