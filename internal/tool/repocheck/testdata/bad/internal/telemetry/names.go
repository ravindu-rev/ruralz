package telemetry

import "context"

const requests = "ruralz_http_requests_total"

const span = "ruralz.filter.x"

func start(ctx context.Context, tr tracer) { tr.Start(ctx, "custom") }
