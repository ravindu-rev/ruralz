# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# The Compose suite's OpenTelemetry Collector
# (docs/engineering/03-testing-and-quality-strategy.md "Compose suite"):
# the core distribution pinned by the digest of its multi-arch index
# (docs/engineering/01-tech-stack-and-libraries.md "CI tooling"), which
# Dependabot's docker updates move, with collector.yaml as its
# configuration: an OTLP/gRPC receiver over TLS and one file exporter per
# signal. The suite builds it from test/compose; the context admits only
# collector.yaml (collector.Dockerfile.dockerignore). It is never published.

FROM otel/opentelemetry-collector:0.164.0@sha256:28b6d41ac51bff6b41ba5e8fe503569ffb9d6a1ea90f7a78419e86f34e44f750
COPY --chmod=0644 collector.yaml /etc/otelcol/config.yaml
