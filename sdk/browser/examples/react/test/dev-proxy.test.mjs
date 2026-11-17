import assert from "node:assert/strict";
import { createServer as createHTTPServer } from "node:http";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { createServer as createViteServer } from "vite";

const exampleRoot = path.dirname(fileURLToPath(new URL("../package.json", import.meta.url)));

async function availablePort() {
  const reservation = createHTTPServer();
  await new Promise((resolve) => reservation.listen(0, "127.0.0.1", resolve));
  const address = reservation.address();
  assert(address && typeof address !== "string");
  await new Promise((resolve, reject) => reservation.close((error) => error ? reject(error) : resolve()));
  return address.port;
}

test("React development server forwards journey requests to the configured Go Fiber target", async (t) => {
  let upstreamPath = "";
  const upstream = createHTTPServer((request, response) => {
    upstreamPath = request.url ?? "";
    response.writeHead(200, { "content-type": "text/plain" });
    response.end("go-fiber-journey");
  });
  await new Promise((resolve) => upstream.listen(0, "127.0.0.1", resolve));

  const upstreamAddress = upstream.address();
  assert(upstreamAddress && typeof upstreamAddress !== "string");
  const previousTarget = process.env.LO_E2E_JOURNEY_TARGET;
  process.env.LO_E2E_JOURNEY_TARGET = `http://127.0.0.1:${upstreamAddress.port}`;

  let vite;
  t.after(async () => {
    if (vite) await vite.close();
    await new Promise((resolve, reject) => upstream.close((error) => error ? reject(error) : resolve()));
    if (previousTarget === undefined) delete process.env.LO_E2E_JOURNEY_TARGET;
    else process.env.LO_E2E_JOURNEY_TARGET = previousTarget;
  });

  const vitePort = await availablePort();
  vite = await createViteServer({
    configFile: path.join(exampleRoot, "vite.config.ts"),
    logLevel: "silent",
    root: exampleRoot,
    server: { host: "127.0.0.1", port: vitePort, strictPort: true },
  });
  await vite.listen();

  const viteAddress = vite.httpServer?.address();
  assert(viteAddress && typeof viteAddress !== "string");
  const response = await fetch(
    `http://127.0.0.1:${viteAddress.port}/journey-api/performance/error?source=react-dev`,
  );

  assert.deepEqual(
    { body: await response.text(), status: response.status, upstreamPath },
    {
      body: "go-fiber-journey",
      status: 200,
      upstreamPath: "/performance/error?source=react-dev",
    },
  );
});
