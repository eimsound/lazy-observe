import { init } from "@lazy-observe/browser";
import { ErrorBoundary, useRouteTracking } from "@lazy-observe/react";
import React, { useState } from "react";
import { createRoot } from "react-dom/client";

declare const __LAZY_OBSERVE_RELEASE__: string;

const configuredDsn = new URLSearchParams(location.search).get("dsn");
const dsn = configuredDsn ?? "http://127.0.0.1:3100/p/default@replace-me";
if (configuredDsn) history.replaceState(null, "", location.pathname);
const client = init({
  dsn,
  serviceName: "lazy-observe-browser-journey",
  release: __LAZY_OBSERVE_RELEASE__,
  errorBatchSize: 1,
  errorFlushIntervalMs: 250,
  replay: { onErrorOnly: true },
});

function Crash(): never { throw new Error("React demo render error"); }

function createCheckoutFailure(status: number): Error {
  return new Error(`Checkout request failed with status ${status}`);
}

function App() {
  const [broken, setBroken] = useState(false);
  const [journeyResult, setJourneyResult] = useState("Not started");
  const [outsideResult, setOutsideResult] = useState("Not started");
  const [paymentSecret, setPaymentSecret] = useState("");
  useRouteTracking(location.pathname);
  if (broken) return <Crash />;

  async function runErrorJourney() {
    let eventId = "";
    let traceId = "";
    await client.startSpan({ name: "submit checkout" }, async () => {
      const response = await fetch(
        `/journey-api/performance/error?payment_secret=${encodeURIComponent(paymentSecret)}`,
      );
      const body = await response.json() as { trace_id?: string };
      traceId = body.trace_id ?? "";
      eventId = client.captureException(createCheckoutFailure(response.status)) ?? "";
    });
    await client.flush();
    setJourneyResult(`Event: ${eventId}\nTrace: ${traceId}`);
  }

  async function captureOutsideScope() {
    const eventId = client.captureException(new Error("Outside completed trace scope")) ?? "";
    await client.flush();
    setOutsideResult(`Event: ${eventId}`);
  }

  return (
    <main>
      <h1>lazy-observe React demo</h1>
      <p>Session: <output aria-label="Session ID">{client.sessionId}</output></p>
      <label>
        Payment secret
        <input
          aria-label="Payment secret"
          autoComplete="off"
          type="text"
          value={paymentSecret}
          onChange={(event) => setPaymentSecret(event.currentTarget.value)}
        />
      </label>
      <button onClick={() => setBroken(true)}>Crash render</button>
      <button onClick={() => void runErrorJourney()}>Run error journey</button>
      <output aria-label="Error journey result">{journeyResult}</output>
      <button onClick={() => void captureOutsideScope()}>Capture outside scope</button>
      <output aria-label="Outside scope result">{outsideResult}</output>
    </main>
  );
}

createRoot(document.getElementById("root")!).render(<ErrorBoundary fallback={<p>Captured by ErrorBoundary.</p>}><App /></ErrorBoundary>);
