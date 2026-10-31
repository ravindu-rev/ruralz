# Other fixture

The Node counts `ruralz_fixture_requests_total` and `ruralz_bogus_total`; histograms expose `ruralz_fixture_latency_seconds_bucket`, and `ruralz_fixture_*` is a prefix.

It raises the degraded reason `bogus_reason` and the degraded state `fixture_ok`.

```promql
ruralz_node_degraded_info{reason=~"fixture_ok|fixture_unknown"} == 1
```
