import { init } from "@lazy-observe/browser";

const dsn = new URLSearchParams(location.search).get("dsn") ?? "http://127.0.0.1:3100/p/default@replace-me";
const client = init({
  dsn,
  errorBatchSize: 1,
  errorFlushIntervalMs: 250,
  replay: { onErrorOnly: true },
});
const output = document.querySelector<HTMLPreElement>("#output")!;
output.textContent = `Session: ${client.sessionId}`;

document.querySelector("#error")!.addEventListener("click", async () => {
  client.captureException(new Error("vanilla demo error"));
  await client.flush();
  output.textContent = "Error captured";
});
document.querySelector("#request")!.addEventListener("click", async () => {
  const response = await fetch("/");
  output.textContent = `Request: ${response.status}`;
});
