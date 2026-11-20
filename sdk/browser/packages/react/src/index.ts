import { captureException, setPage, type CaptureOptions } from "@lazy-observe/browser";
import React, { type ErrorInfo, type ReactNode, useEffect } from "react";

export interface ErrorBoundaryProps {
  children: ReactNode;
  fallback?: ReactNode | ((error: Error, reset: () => void) => ReactNode);
  onError?: (error: Error, info: ErrorInfo) => void;
  onReset?: () => void;
  resetKeys?: readonly unknown[];
  capture?: (error: unknown, options?: CaptureOptions) => string | undefined;
}

interface ErrorBoundaryState {
  hasError: boolean;
  error?: unknown;
}

function normalizeError(value: unknown): Error {
  if (value instanceof Error) return value;
  return new Error(value === undefined || value === null || value === "" ? "Unknown error" : String(value));
}

function resetKeysChanged(prev?: readonly unknown[], next?: readonly unknown[]): boolean {
  if (prev === next) return false;
  if (!prev || !next || prev.length !== next.length) return true;
  for (let i = 0; i < prev.length; i++) {
    if (!Object.is(prev[i], next[i])) return true;
  }
  return false;
}

export class ErrorBoundary extends React.Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { hasError: false };
  static getDerivedStateFromError(error: unknown): ErrorBoundaryState { return { hasError: true, error }; }
  componentDidCatch(error: unknown, info: ErrorInfo): void {
    (this.props.capture ?? captureException)(error, { tags: { "react.component_stack": info.componentStack ?? "" } });
    this.props.onError?.(normalizeError(error), info);
  }
  componentDidUpdate(prevProps: ErrorBoundaryProps): void {
    if (this.state.hasError && resetKeysChanged(prevProps.resetKeys, this.props.resetKeys)) {
      this.reset();
    }
  }
  reset = (): void => {
    if (!this.state.hasError) return;
    this.props.onReset?.();
    this.setState({ hasError: false, error: undefined });
  };
  render(): ReactNode {
    if (!this.state.hasError) return this.props.children;
    if (typeof this.props.fallback === "function") {
      return this.props.fallback(normalizeError(this.state.error), this.reset);
    }
    return this.props.fallback ?? null;
  }
}

export function useRouteTracking(page?: string): void {
  useEffect(() => {
    const track = () => setPage(page ?? window.location.href);
    track();
    if (page !== undefined) return;
    window.addEventListener("popstate", track);
    window.addEventListener("hashchange", track);
    return () => { window.removeEventListener("popstate", track); window.removeEventListener("hashchange", track); };
  }, [page]);
}
