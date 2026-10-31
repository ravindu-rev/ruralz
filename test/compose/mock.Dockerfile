# Copyright 2026 Revington
# SPDX-License-Identifier: Apache-2.0
#
# The Compose suite's test-only mock image
# (docs/engineering/03-testing-and-quality-strategy.md "Compose suite"):
# the static mock binary, which the suite builds from test/compose/mockimage
# with CGO_ENABLED=0 for the runner's architecture, alone on a scratch base.
# It serves as the mock Upstream, the mock IdP, a DNS sink and a probe
# client (test/compose/mock), runs as user and group 65532 and is never
# published. The suite builds it with a context holding only the binary,
# named mock.

FROM scratch
COPY --chmod=0755 mock /mock
USER 65532:65532
ENTRYPOINT ["/mock"]
