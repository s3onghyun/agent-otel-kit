# Contributing

Before opening a pull request, run the checks CI runs:

```sh
make lint test                 # gofmt, go vet, go test
make validate                  # compose files and collector config
make check-collector-warnings  # collector start-up log (needs docker)
make test-collector            # collector rules on recorded payloads (needs docker)
```

If you change a dashboard, edit `tools/gendashboards` and run `make dashboards`.

## Collector config changes

- Check claims about a component (exists, core or contrib, removed, deprecated) on the pinned image: `docker run --rm <image> components`, where `<image>` is `OTELCOL_IMAGE` from the Makefile.
- Run `make validate` and `make check-collector-warnings`.
- Use the current snake_case type names (`otlp_grpc`, `otlp_http`, `delta_to_cumulative`, `signal_to_metrics`, ...), not the older aliases.
- Keep content capture off by default: new content attributes go into `transform/redact`, behind `AGENT_OTEL_KIT_CAPTURE_CONTENT`.
- `otelcol validate` is silent about deprecated aliases and keys; they show up only as warnings when the collector starts, which is what `make check-collector-warnings` looks for.
