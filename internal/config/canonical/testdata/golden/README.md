# Encoder golden files

`*.parsed.json` and `*.parsed.digest` hold the `ruralz.canonical.v1` bytes and digest that the encoder writes for the parsed trees of `../bundles/<name>` (YAML, and its JSON twin), before stage G materializes defaults and normalizes scalars. They pin the encoder, not the Revision of a Bundle: the golden corpus of pipeline output, the `shop-bundle` Revision included, lives in `test/conformance/config` (WP-75, spec 02 req 79). Regenerate with `go test -run Golden -update`.
