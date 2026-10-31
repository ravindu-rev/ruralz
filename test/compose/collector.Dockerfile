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

FROM otel/opentelemetry-collector:0.163.2-386@sha256:6af85116b470638e039d957f130990f0de7aaa46e51863e8c133e83590758992
COPY --chmod=0644 collector.yaml /etc/otelcol/config.yaml
