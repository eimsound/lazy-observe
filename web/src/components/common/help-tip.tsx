import { Info } from "lucide-react";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

export interface HelpTipScopeItem {
  text: string;
  children?: string[];
}

type HelpTipProps =
  | {
      content: string;
      scope?: undefined;
      scopeTitle?: undefined;
      side?: "top" | "right" | "bottom" | "left";
    }
  | {
      content?: undefined;
      scope: HelpTipScopeItem[];
      scopeTitle: string;
      side?: "top" | "right" | "bottom" | "left";
    };

/** Compact hover-triggered help icon (`tabIndex={-1}`) that moves explanatory copy out of permanent layout. */
export function HelpTip(props: HelpTipProps) {
  const ariaLabel =
    props.scope !== undefined
      ? [
          props.scopeTitle,
          ...props.scope.flatMap((item) => [item.text, ...(item.children ?? [])]),
        ].join(" ")
      : props.content;

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        {/* Exclude from tab order so Radix Dialog auto-focus does not open the tooltip unprompted. */}
        <button
          type="button"
          tabIndex={-1}
          className="inline-flex size-4 shrink-0 items-center justify-center rounded-sm text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          aria-label={ariaLabel}
        >
          <Info className="size-3.5" />
        </button>
      </TooltipTrigger>
      <TooltipContent
        side={props.side}
        className="max-w-xs whitespace-normal text-left leading-relaxed"
      >
        {props.scope !== undefined ? (
          <div className="space-y-1 text-left">
            <p className="font-medium">{props.scopeTitle}</p>
            <ul className="list-disc space-y-0.5 pl-4">
              {props.scope.map((item) => (
                <li key={item.text}>
                  <span>{item.text}</span>
                  {item.children && item.children.length > 0 && (
                    <ul className="mt-0.5 list-[circle] space-y-0.5 pl-4">
                      {item.children.map((child) => (
                        <li key={child}>{child}</li>
                      ))}
                    </ul>
                  )}
                </li>
              ))}
            </ul>
          </div>
        ) : (
          props.content
        )}
      </TooltipContent>
    </Tooltip>
  );
}
