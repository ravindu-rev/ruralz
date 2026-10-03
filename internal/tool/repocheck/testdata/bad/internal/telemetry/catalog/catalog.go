package catalog

import "context"

// The catalog spells names: none of these is a finding.
const requests = "ruralz_http_requests_total"

const span = "ruralz.filter.x"

func start(ctx context.Context, tr tracer) { tr.Start(ctx, "custom") }
