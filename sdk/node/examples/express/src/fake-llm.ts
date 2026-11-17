import { createServer } from "node:http";

const server = createServer((req, res) => {
  if (req.url === "/healthz") { res.end("ok"); return; }
  if (req.url !== "/v1/chat/completions" || req.method !== "POST") { res.writeHead(404).end(); return; }
  let body = "";
  req.setEncoding("utf8");
  req.on("data", (chunk) => { body += chunk; });
  req.on("end", () => {
    const request = JSON.parse(body) as { stream?: boolean };
    if (request.stream) {
      res.writeHead(200, { "content-type": "text/event-stream" });
      res.write('data: {"model":"e2e-response-model","choices":[{"delta":{"content":"streamed "}}]}\n\n');
      res.end('data: {"choices":[{"delta":{"content":"completion"}}],"usage":{"prompt_tokens":23,"completion_tokens":4}}\n\ndata: [DONE]\n\n');
      return;
    }
    res.writeHead(200, { "content-type": "application/json" });
    res.end('{"model":"e2e-response-model","choices":[{"message":{"content":"fixed completion"}}],"usage":{"prompt_tokens":23,"completion_tokens":4}}');
  });
});
server.listen(Number(process.env.PORT ?? 3430), "127.0.0.1");
const stop = () => server.close();
process.once("SIGTERM", stop);
process.once("SIGINT", stop);
