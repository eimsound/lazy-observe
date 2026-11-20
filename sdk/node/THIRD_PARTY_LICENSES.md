# Third-party licenses

Direct production dependencies:

| Package | Use | License |
|---|---|---|
| `@opentelemetry/api` | telemetry APIs (trace, metrics, logs) | Apache-2.0 |
| `@opentelemetry/context-async-hooks` and `@opentelemetry/core` | explicit AsyncLocalStorage context and W3C propagation | Apache-2.0 |
| `@opentelemetry/exporter-{logs,metrics,trace}-otlp-proto` | OTLP/HTTP protobuf export | Apache-2.0 |
| `@opentelemetry/resources` | telemetry resource | Apache-2.0 |
| `@opentelemetry/sdk-{logs,metrics,trace}` | manually assembled providers | Apache-2.0 |
| `@opentelemetry/semantic-conventions` | HTTP/resource attribute names | Apache-2.0 |
| `pprof-format` | pure-JavaScript pprof encoding | MIT |

Optional peer dependencies (loaded lazily only when the corresponding integration is invoked):

| Package | Use | License |
|---|---|---|
| `express` | optional Express middleware integration (`lo.express()`, `lo.expressError()`) | MIT |
| `pino` | optional Pino logger integration (`lo.pino()`) | MIT |
| `winston` / `winston-transport` | optional Winston transport integration (`lo.winston()`) | MIT |

Development-only dependencies are not shipped with the package. Run `pnpm licenses list --prod` from this directory to audit the resolved dependency tree.
