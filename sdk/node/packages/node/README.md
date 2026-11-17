# @lazy-observe/node

Explicit Express middleware, Pino/Winston logging, error events, and runtime metrics for lazy-observe. Importing the package has no side effects; call `init()` and install each integration explicitly.

```ts
import { init } from "@lazy-observe/node";

const lo = init({ dsn: "http://host:3100/p/myapp@token" });
app.use(lo.express());
const logger = lo.pino();
app.use(lo.expressError());
```

See the workspace-level README for the complete configuration and API reference.
Custom errors and profiles use configurable bounded queues; `reportError()` does
not await the network, and `shutdown({timeoutMs})` drains or returns the latest
export failure within the caller's budget.
