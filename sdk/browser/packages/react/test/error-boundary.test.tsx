import { render, screen } from "@testing-library/react";
import { createTraceContext, init, type ErrorEventPayload } from "@lazy-observe/browser";
import { expect, it, vi } from "vitest";
import { ErrorBoundary } from "../src";

function Broken(): never { throw new Error("render failed"); }

it("captures rendering errors and displays a custom fallback", () => {
  const capture = vi.fn((_error: unknown) => "event-id");
  const consoleError = vi.spyOn(console, "error").mockImplementation(() => undefined);
  render(<ErrorBoundary capture={capture} fallback={(error) => <p>Failed: {error.message}</p>}><Broken /></ErrorBoundary>);
  expect(screen.getByText("Failed: render failed")).toBeTruthy();
  expect(capture).toHaveBeenCalledTimes(1);
  expect(capture.mock.calls[0][0]).toBeInstanceOf(Error);
  consoleError.mockRestore();
});

it("forwards an explicit trace context for delayed framework capture", async () => {
  const captured: ErrorEventPayload[] = [];
  const client = init({
    dsn: "https://observe.example/p/project@token",
    errors: false,
    fetchTracing: false,
    replay: false,
    vitals: false,
    beforeSend(event) {
      captured.push(event);
      return undefined;
    },
  });
  const trace = createTraceContext();
  const consoleError = vi.spyOn(console, "error").mockImplementation(() => undefined);

  render(
    <ErrorBoundary
      capture={(error, options) => client.captureException(error, { ...options, trace })}
      fallback={<p>Failed</p>}
    >
      <Broken />
    </ErrorBoundary>,
  );

  expect(captured[0].contexts?.trace).toEqual(trace);
  consoleError.mockRestore();
  await client.close();
});
