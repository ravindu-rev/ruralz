# Observability fixture

### Ruralz Gateway metrics

| Metric | Type | Unit | Labels | Meaning or values |
|---|---|---|---|---|
| `ruralz_fixture_requests_total` | Counter | requests | `route`, `status_class` | Matches the catalog |
| `ruralz_x_count` | Counter | requests | none | Breaks the counter suffix rule |
| `ruralz_fixture_latency_seconds` | Histogram, request | seconds | `listener` | Wrong bound set |
| `ruralz_fixture_open_connections` | Gauge | connections | `listener`, `protocol` | Label not in the catalog |
| `ruralz_fixture_queue_items` | Gauge | entries | none | Wrong unit |
| `ruralz_fixture_errors_total` | Gauge | errors | `code` | Wrong type |
| `ruralz_node_degraded_info` | Gauge | info | `reason` | Not in the fixture catalog, so not compared |
| `ruralz_fixture_a_items`, `ruralz_fixture_b_items`, `ruralz_fixture_c_total` | Gauge; Counter | items | none | Type cell with two values for three names |
| `ruralz_fixture_d_items`, `ruralz_fixture_e_items`, `ruralz_fixture_h_total` | Counter; Gauge for e_items | items | none | Default type wrong for d_items |
| `ruralz_fixture_f_items`, `ruralz_fixture_g_total` | Gauge; Counter for g_total | items, cycles, bytes | none | Unit cell with three values for two names |

### AI metrics

| Metric | Type | Unit | Labels | Mirrors, meaning or values |
|---|---|---|---|---|
| `ruralz_fixture_ai_total` | Counter | requests | none | In the catalog, outside the gateway table |

### Degraded states

| `reason` | Raised while |
|---|---|
| `fixture_ok` | Always fine |
| `fixture_later` | Tagged for later; Planned (M2) |

### Other states

| `reason` | Raised while |
|---|---|
| `fixture_elsewhere` | A reason table outside "Degraded states" is not the catalog |
