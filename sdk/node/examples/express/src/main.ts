import express from "express";
import { init } from "@lazy-observe/node";

const lo = init({
  dsn: process.env.LO_DSN ?? "http://127.0.0.1:3100/p/default@development-token",
  serviceName: process.env.LO_SERVICE_NAME ?? "node-express-example",
  exportIntervalMs: 500,
  runtimeMetricsIntervalMs: 250,
  profiling: process.env.LO_PROFILING !== "false",
  captureLLMContent: process.env.LO_CAPTURE_LLM_CONTENT === "true",
});
const app = express();
app.use(lo.express());
const logger = lo.pino();

app.get("/", (_req, res) => { logger.info("hello from express"); res.json({ ok: true }); });
app.get("/hello/:name", (req, res) => { logger.info({ name: req.params.name }, "hello route"); res.json({ hello: req.params.name }); });
app.get("/error", (_req, _res) => { logger.error("about to throw example error"); throw new Error("example route failed"); });
const wait = (milliseconds: number) => new Promise<void>((resolve) => setTimeout(resolve, milliseconds));
const performanceRoute = (name: string, defaultDelay: number, status: number) => async (
  req: express.Request,
  res: express.Response,
) => {
  const rawDelay = typeof req.query.delay_ms === "string" ? Number(req.query.delay_ms) : defaultDelay;
  if (!Number.isInteger(rawDelay) || rawDelay < 1 || rawDelay > 1_000) {
    res.status(400).json({ error: "delay_ms must be between 1 and 1000" });
    return;
  }
  const started = performance.now();
  await wait(rawDelay);
  const durationMs = performance.now() - started;
  logger.info({ workload: name, duration_ms: durationMs, status }, "node performance workload complete");
  res.status(status).json({ workload: name, duration_ms: durationMs });
};
app.get("/performance/fast", performanceRoute("fast", 5, 200));
app.get("/performance/slow", performanceRoute("slow", 150, 200));
app.get("/performance/error", performanceRoute("error", 30, 503));
const llmFetch = lo.wrapLLMFetch();
app.get("/llm", async (req, res, next) => {
  try {
    const stream = req.query.stream === "1";
    const base = process.env.LO_LLM_URL ?? "http://127.0.0.1:3430";
    const response = await llmFetch(`${base.replace(/\/$/, "")}/v1/chat/completions`, {
      method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify({ model: "e2e-request-model", messages: [{ role: "user", content: "e2e secret prompt" }], stream }),
    });
    res.status(response.status);
    res.setHeader("content-type", response.headers.get("content-type") ?? "application/json");
    res.send(Buffer.from(await response.arrayBuffer()));
  } catch (error) { next(error); }
});

app.use(lo.expressError());
app.use((error: Error, _req: express.Request, res: express.Response, _next: express.NextFunction) => {
  res.status(500).json({ error: error.message });
});

const server = app.listen(Number(process.env.PORT ?? 3420), "127.0.0.1", () => logger.info("example listening"));
const retainedProfileAllocations: number[][] = [];
function profileExampleAllocation(): number[] {
  return Array.from({ length: 256 }, (_, index) => index * 3);
}
function profileExampleWork(): void {
  const until = performance.now() + 4;
  let value = 0;
  while (performance.now() < until) value = Math.imul(value + 1, 2654435761);
  const allocation = profileExampleAllocation();
  allocation[0] = value;
  retainedProfileAllocations.push(allocation);
  if (retainedProfileAllocations.length > 128) retainedProfileAllocations.shift();
}
const profileWorkTimer = setInterval(profileExampleWork, 100);
profileWorkTimer.unref();
async function stop() { server.close(); await lo.shutdown(); }
process.once("SIGTERM", () => { clearInterval(profileWorkTimer); void stop(); });
process.once("SIGINT", () => { clearInterval(profileWorkTimer); void stop(); });
