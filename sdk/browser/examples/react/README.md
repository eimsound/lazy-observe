# React example

Start the Go Fiber example on `http://127.0.0.1:3320`, then run the React development server with its journey target:

```fish
env LO_E2E_JOURNEY_TARGET=http://127.0.0.1:3320 mise exec -- pnpm dev
```

Add `?dsn=http://127.0.0.1:3100/p/default@YOUR_TOKEN` to the React URL. **Run error journey** now crosses the public `/journey-api` development proxy and exercises the Go Fiber `/performance/error` route, producing linked browser and server traces. **Crash render** demonstrates `ErrorBoundary`; `useRouteTracking` records the normalized route for Vitals and replay metadata.
