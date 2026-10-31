# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# Container image of Ruralz Gateway, ghcr.io/ravindu-rev/ruralzd
# (docs/engineering/04-release-versioning-and-compatibility.md, "Release
# artifacts" and "Packaging and audit"): the released linux ruralzd binary
# on an empty base, with only the CA bundle taken from a digest-pinned
# golang:1.27.1-bookworm stage (docs/engineering/01-tech-stack-and-libraries.md,
# "Static builds" and "CI tooling"), whose literal FROM line Dependabot's
# docker updates move. `make release-dry-run` builds it from the
# repository root once it has left dist/linux_<arch>/ruralzd and
# dist/notices/ruralzd_linux_<arch>/:
#
#   docker buildx build -f deploy/container/Dockerfile --target ruralzd \
#     --platform linux/amd64,linux/arm64 --provenance=false --sbom=false \
#     --build-arg SOURCE_DATE_EPOCH=<commit time> \
#     --build-arg VERSION=<version> --build-arg COMMIT=<full commit> \
#     --output type=oci,dest=dist/image,tar=false,rewrite-timestamp=true .
#
# Dockerfile.dockerignore limits the context to those inputs and LICENSE.
# Nothing runs on the target platform, so a multi-platform build needs no
# emulation. Every file the image holds is newer than the commit: the
# context's were checked out or built after it, and the certs stage writes
# its own, the CA bundle copied anew rather than kept with the base image's
# time, which differs between its platform variants. rewrite-timestamp
# clamps each to SOURCE_DATE_EPOCH, so a rebuild of the commit on either
# build platform gives the same image digest. The image holds
#
#   /usr/local/bin/ruralzd                        the released binary
#   /etc/ssl/certs/ca-certificates.crt            the CA bundle
#   /usr/share/doc/ruralzd/LICENSE                the repository LICENSE
#   /usr/share/doc/ruralzd/NOTICE                 the artifact NOTICE (depgate)
#   /usr/share/doc/ruralzd/THIRD_PARTY_LICENSES   the binary's (depgate) plus
#                                                 the CA bundle's (MPL-2.0)
#   /var/lib/ruralz                               the data directory, 0700
#
# and every other directory at 0755, owned by root, so user and group
# 65532, as which it runs, never root, can reach the files. Set
# RURALZ_CONFIG; give the container a stop grace period above the 30 s
# Drain exit bound (target).

# The CA bundle, its notice, the directories and the data directory are the
# same on every platform, so this stage runs on the build platform.
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm@sha256:8d48e12ec56735e9358640898b9d9b9fcca110612ed8a5567438c0a1baa24e66 AS certs
ARG TARGETARCH
COPY dist/notices/ruralzd_linux_${TARGETARCH}/THIRD_PARTY_LICENSES /work/THIRD_PARTY_LICENSES.binary
RUN set -eu; \
    version="$(dpkg-query --showformat='${Version}' --show ca-certificates)"; \
    { \
      cat /work/THIRD_PARTY_LICENSES.binary; \
      printf '\n%s\n' '================================================================================'; \
      printf 'ca-certificates %s (Debian): /etc/ssl/certs/ca-certificates.crt\n' "$version"; \
      printf 'License: MPL-2.0 (certificate data)\n'; \
      printf 'Source: https://sources.debian.org/src/ca-certificates/%s/\n' "$version"; \
      printf 'MPL-2.0 Source Code Form: the Source address above.\n'; \
      printf '%s\n' '--------------------------------------------------------------------------------'; \
      printf 'copyright:\n\n'; \
      cat /usr/share/doc/ca-certificates/copyright; \
    } > /work/THIRD_PARTY_LICENSES; \
    cp /etc/ssl/certs/ca-certificates.crt /work/ca-certificates.crt; \
    mkdir -p /work/dirs/etc/ssl/certs /work/dirs/usr/local/bin /work/dirs/usr/share/doc/ruralzd /work/dirs/var/lib; \
    chmod -R 0755 /work/dirs; \
    install -d -m 0700 /work/varlib/ruralz

FROM scratch AS ruralzd
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
# The directories come first: a COPY --chmod gives a directory it creates
# its mode, so a 0644 file would leave its directories untraversable, and
# a COPY --chown gives one its owner.
COPY --from=certs /work/dirs/ /
COPY --from=certs --chmod=0644 /work/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chmod=0755 dist/linux_${TARGETARCH}/ruralzd /usr/local/bin/ruralzd
COPY --chmod=0644 LICENSE /usr/share/doc/ruralzd/LICENSE
COPY --chmod=0644 dist/notices/ruralzd_linux_${TARGETARCH}/NOTICE /usr/share/doc/ruralzd/NOTICE
COPY --from=certs --chmod=0644 /work/THIRD_PARTY_LICENSES /usr/share/doc/ruralzd/THIRD_PARTY_LICENSES
COPY --from=certs --chown=65532:65532 /work/varlib/ /var/lib/
LABEL org.opencontainers.image.title="ruralzd" \
      org.opencontainers.image.description="Ruralz Gateway" \
      org.opencontainers.image.source="https://github.com/ravindu-rev/ruralz" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.vendor="Revington" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
USER 65532:65532
EXPOSE 8080/tcp 8443/tcp 9901/tcp
ENTRYPOINT ["/usr/local/bin/ruralzd"]
