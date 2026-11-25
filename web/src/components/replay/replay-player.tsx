import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ExternalLink, Pause, Play } from "lucide-react";
import type {
  ReplayMarker,
  ReplayMarkerType,
  ReplayErrorMarker,
  RRWebEvent,
} from "@/lib/api/replay";
import { formatClock, formatClockDuration } from "@/lib/format";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { Button } from "@/components/ui/button";
import { IssueLevelDot } from "@/components/issues/issue-level-badge";

/**
 * Session-replay player. Drives rrweb's `Replayer` engine directly and wraps it
 * in a controller (play/pause, seek, speed) built entirely from our own theme
 * tokens — so there is no third-party player chrome to fight and no hardcoded
 * colors. Below the scrub bar sits a filterable marker strip for errors, safe
 * HTTP request metadata and Web Vitals. Clicking any marker seeks the player;
 * resolved Issue markers also link to their Issue.
 *
 * (We drive the engine directly rather than rrweb-player because rrweb-player
 * 2.x's Svelte component fails to instantiate its Replayer when mounted from
 * React; this gives full control over both playback and theming.)
 */

const RECORDED_W = 1280;
const RECORDED_H = 720;
const SPEEDS = [1, 2, 4, 8];
type ReplayEngine = {
  wrapper: HTMLDivElement;
  destroy: () => void;
  getCurrentTime: () => number;
  getMetaData: () => { totalTime?: number };
  on: (event: "finish", cb: () => void) => void;
  pause: (timeOffset?: number) => void;
  play: (timeOffset?: number) => void;
  setConfig: (config: { speed: number }) => void;
};

const MARKER_TYPES: ReplayMarkerType[] = ["error", "http", "vital"];

function errorColor(level: ReplayErrorMarker["level"]): string {
  return level === "error" ? "var(--level-error)" : level === "warning" ? "var(--level-warn)" : "var(--level-info)";
}

function markerColor(marker: ReplayMarker): string {
  if (marker.type === "error") return errorColor(marker.level);
  if (marker.type === "http") return marker.status === 0 || marker.status >= 400 ? "var(--level-error)" : "var(--primary)";
  return marker.rating === "poor" ? "var(--level-error)" : marker.rating === "needs-improvement" ? "var(--level-warn)" : "var(--level-info)";
}

function markerTitle(marker: ReplayMarker): string {
  if (marker.type === "error") return marker.title;
  if (marker.type === "http") {
    const status = marker.status === 0 ? "ERR" : String(marker.status);
    return `${marker.method} ${marker.path} · ${status} · ${Math.round(marker.duration_ms)} ms`;
  }
  const value = marker.name === "CLS" ? marker.value.toFixed(3) : `${Math.round(marker.value)} ms`;
  return `${marker.name} · ${value}`;
}

function markerKey(marker: ReplayMarker, index: number): string {
  return marker.type === "error" && marker.event_id
    ? `${marker.type}-${marker.event_id}`
    : `${marker.type}-${marker.time}-${index}`;
}

export function ReplayPlayer({
  events,
  startedAt,
  durationMs,
  markers,
  onOpenIssue,
}: {
  events: RRWebEvent[];
  startedAt: number;
  durationMs: number;
  markers: ReplayMarker[];
  onOpenIssue: (issueId: string, eventId: string) => void;
}) {
  const { t } = useTranslation();
  const hostRef = useRef<HTMLDivElement>(null);
  const replayerRef = useRef<ReplayEngine | null>(null);

  const [playing, setPlaying] = useState(false);
  const [currentTime, setCurrentTime] = useState(0);
  const [totalTime, setTotalTime] = useState(durationMs);
  const [speed, setSpeed] = useState(1);
  const speedRef = useRef(speed);
  const [playerUnavailable, setPlayerUnavailable] = useState(false);
  const [visibleTypes, setVisibleTypes] = useState<Set<ReplayMarkerType>>(
    () => new Set(MARKER_TYPES),
  );

  // Create the replayer once for the loaded event stream (do not depend on `t`
  // so switching UI language does not destroy/rebuild the engine).
  useEffect(() => {
    const host = hostRef.current;
    if (!host || events.length < 2) return;
    host.innerHTML = "";
    setPlayerUnavailable(false);
    setPlaying(false);
    setCurrentTime(0);
    let cancelled = false;
    let replayer: ReplayEngine | null = null;
    let ro: ResizeObserver | null = null;

    void (async () => {
      try {
        await import("rrweb/dist/style.css").catch(() => undefined);
        const rrweb = await import("rrweb");
        if (cancelled) return;
        replayer = new rrweb.Replayer(events, {
          root: host,
          speed: speedRef.current,
          skipInactive: false,
          showWarning: false,
          showDebug: false,
          mouseTail: false,
        }) as ReplayEngine;
        replayerRef.current = replayer;

        const meta = replayer.getMetaData();
        setTotalTime(meta.totalTime || durationMs);
        replayer.pause(0);

        const fit = () => {
          if (!replayer) return;
          const available = host.clientWidth || host.parentElement?.clientWidth || 900;
          const scale = Math.min(1.5, available / RECORDED_W);
          replayer.wrapper.style.transform = `scale(${scale})`;
          replayer.wrapper.style.transformOrigin = "top left";
          host.style.height = `${RECORDED_H * scale}px`;
        };
        fit();
        ro = new ResizeObserver(fit);
        ro.observe(host);
        replayer.on("finish", () => setPlaying(false));
      } catch {
        if (cancelled) return;
        setPlayerUnavailable(true);
      }
    })();

    return () => {
      cancelled = true;
      ro?.disconnect();
      try {
        replayer?.destroy();
      } catch {
        /* ignore */
      }
      replayerRef.current = null;
      host.innerHTML = "";
    };
  }, [events, durationMs]);

  // Track playback position with a rAF loop while playing.
  useEffect(() => {
    if (!playing) return;
    let raf = 0;
    const tick = () => {
      const r = replayerRef.current;
      if (r) setCurrentTime(Math.min(totalTime, r.getCurrentTime()));
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [playing, totalTime]);

  function togglePlay() {
    const r = replayerRef.current;
    if (!r) return;
    if (playing) {
      r.pause();
      setPlaying(false);
    } else {
      r.play(currentTime >= totalTime ? 0 : currentTime);
      setPlaying(true);
    }
  }

  function seek(offset: number) {
    const r = replayerRef.current;
    if (!r) return;
    const clamped = Math.min(totalTime, Math.max(0, offset));
    setCurrentTime(clamped);
    if (playing) r.play(clamped);
    else r.pause(clamped);
  }

  function changeSpeed(next: number) {
    speedRef.current = next;
    setSpeed(next);
    replayerRef.current?.setConfig({ speed: next });
  }

  function seekFromTrack(e: React.MouseEvent<HTMLDivElement>) {
    const rect = e.currentTarget.getBoundingClientRect();
    const ratio = (e.clientX - rect.left) / rect.width;
    seek(ratio * totalTime);
  }

  const markerCounts = useMemo(() => {
    const counts: Record<ReplayMarkerType, number> = { error: 0, http: 0, vital: 0 };
    for (const marker of markers) counts[marker.type] += 1;
    return counts;
  }, [markers]);
  const sortedMarkers = useMemo(
    () => markers.filter((marker) => visibleTypes.has(marker.type)).sort((a, b) => a.time - b.time),
    [markers, visibleTypes],
  );
  const progressPct = totalTime > 0 ? Math.min(100, (currentTime / totalTime) * 100) : 0;
  const markerDuration = Math.max(1, durationMs);

  function toggleMarkerType(type: ReplayMarkerType) {
    setVisibleTypes((previous) => {
      const next = new Set(previous);
      if (next.has(type)) next.delete(type);
      else next.add(type);
      return next;
    });
  }

  if (playerUnavailable) {
    return (
      <div className="py-8 text-center text-sm text-destructive">
        {t("replays.playerUnavailable")}
      </div>
    );
  }

  return (
    <div className="space-y-3">
      {/* Replay surface */}
      <div data-lo-block className="overflow-hidden rounded-md border border-border bg-muted/30">
        <div ref={hostRef} className="lo-rrweb w-full overflow-hidden" />
      </div>

      {/* Controls */}
      <div className="flex items-center gap-3">
        <Button size="icon" variant="outline" className="size-9" onClick={togglePlay}>
          {playing ? <Pause className="size-4" /> : <Play className="size-4" />}
        </Button>
        <span className="shrink-0 font-mono text-xs tabular-nums text-muted-foreground">
          {formatClockDuration(currentTime)} / {formatClockDuration(totalTime)}
        </span>

        {/* Scrub + marker track */}
        <div className="flex-1">
          <div
            role="slider"
            aria-valuemin={0}
            aria-valuemax={Math.round(totalTime)}
            aria-valuenow={Math.round(currentTime)}
            tabIndex={0}
            onClick={seekFromTrack}
            className="relative h-2.5 w-full cursor-pointer rounded-full bg-muted"
          >
            <div
              className="absolute inset-y-0 left-0 rounded-full bg-primary"
              style={{ width: `${progressPct}%` }}
            />
            {sortedMarkers.map((m, index) => {
              const pct = Math.min(100, Math.max(0, ((m.time - startedAt) / markerDuration) * 100));
              return (
                <span
                  key={`tick-${markerKey(m, index)}`}
                  className="absolute top-1/2 h-3.5 w-[3px] -translate-x-1/2 -translate-y-1/2 rounded-full"
                  style={{ left: `${pct}%`, backgroundColor: markerColor(m) }}
                />
              );
            })}
          </div>
        </div>

        <div className="flex shrink-0 gap-1">
          {SPEEDS.map((s) => (
            <button
              key={s}
              type="button"
              onClick={() => changeSpeed(s)}
              className={
                "rounded px-1.5 py-0.5 text-xs font-medium " +
                (speed === s
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground")
              }
            >
              {s}×
            </button>
          ))}
        </div>
      </div>

      {/* Structured timeline markers */}
      <div>
        <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
          <div className="text-xs font-medium text-muted-foreground">
            {t("replays.timelineMarkers")}
          </div>
          <div className="flex flex-wrap gap-1" aria-label={t("replays.markerFilters")}>
            {MARKER_TYPES.map((type) => (
              <Button
                key={type}
                type="button"
                size="sm"
                variant={visibleTypes.has(type) ? "secondary" : "outline"}
                className="h-7 px-2 text-xs"
                aria-pressed={visibleTypes.has(type)}
                onClick={() => toggleMarkerType(type)}
              >
                {t(`replays.markerType.${type}`)} ({markerCounts[type]})
              </Button>
            ))}
          </div>
        </div>
        {sortedMarkers.length === 0 ? (
          <div className="rounded-md border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
            {markers.length === 0 ? t("replays.noMarkers") : t("replays.noFilteredMarkers")}
          </div>
        ) : (
          <div className="space-y-1.5">
            {sortedMarkers.map((m, index) => (
              <div
                key={`row-${markerKey(m, index)}`}
                className="flex items-center gap-3 rounded-md border border-border px-3 py-2 text-sm"
              >
                {m.type === "error" ? (
                  <IssueLevelDot level={m.level} />
                ) : (
                  <span
                    className="size-2 shrink-0 rounded-full"
                    style={{ backgroundColor: markerColor(m) }}
                  />
                )}
                <Tooltip>
                  <TooltipTrigger asChild>
                    <button
                      type="button"
                      onClick={() => seek(m.time - startedAt)}
                      className="min-w-0 flex-1 truncate text-left font-medium hover:underline"
                      title={markerTitle(m)}
                    >
                      {markerTitle(m)}
                    </button>
                  </TooltipTrigger>
                  <TooltipContent>{formatClock(m.time)}</TooltipContent>
                </Tooltip>
                <span className="shrink-0 font-mono text-xs text-muted-foreground">
                  {formatClock(m.time)}
                </span>
                {m.type === "error" && m.issue_id && m.event_id && (
                  <Button
                    size="sm"
                    variant="outline"
                    className="shrink-0"
                    onClick={() => onOpenIssue(m.issue_id!, m.event_id!)}
                  >
                    <ExternalLink className="size-3.5" />
                    {t("replays.openIssue")}
                  </Button>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
