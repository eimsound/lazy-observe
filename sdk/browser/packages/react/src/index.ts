import { captureException, setPage, type CaptureOptions } from "@lazy-observe/browser";
import React, { type ErrorInfo, type ReactNode, useEffect } from "react";

export interface ErrorBoundaryProps {
  children: ReactNode;
  fallback?: ReactNode | ((error: Error) => ReactNode);
  onError?: (error: Error, info: ErrorInfo) => void;
  capture?: (error: unknown, options?: CaptureOptions) => string | undefined;
}

interface ErrorBoundaryState { error?: Error }

export class ErrorBoundary extends React.Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = {};
  static getDerivedStateFromError(error: Error): ErrorBoundaryState { return { error }; }
  componentDidCatch(error: Error, info: ErrorInfo): void {
    (this.props.capture ?? captureException)(error, { tags: { "react.component_stack": info.componentStack ?? "" } });
    this.props.onError?.(error, info);
  }
  render(): ReactNode {
    if (!this.state.error) return this.props.children;
    if (typeof this.props.fallback === "function") return this.props.fallback(this.state.error);
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
