# Third-party licenses

Direct production dependencies:

| Package | Use | License |
|---|---|---|
| `@opentelemetry/api` and `@opentelemetry/api-logs` | telemetry APIs | Apache-2.0 |
| `@opentelemetry/context-async-hooks` and `@opentelemetry/core` | explicit AsyncLocalStorage context and W3C propagation | Apache-2.0 |
| `@opentelemetry/exporter-{logs,metrics,trace}-otlp-proto` | OTLP/HTTP protobuf export | Apache-2.0 |
| `@opentelemetry/resources` | telemetry resource | Apache-2.0 |
| `@opentelemetry/sdk-{logs,metrics,trace-node}` | manually assembled providers | Apache-2.0 |
| `@opentelemetry/semantic-conventions` | HTTP/resource attribute names | Apache-2.0 |
| `pino` | explicit logger integration | MIT |
| `pprof-format` | pure-JavaScript pprof encoding | MIT |
| `winston-transport` | explicit Winston transport | MIT |

Development-only dependencies are not shipped with the package. Run `pnpm licenses list --prod` from this directory to audit the resolved dependency tree.
