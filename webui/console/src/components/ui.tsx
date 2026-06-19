import type { CSSProperties, ReactNode } from "react";

/** A pulsing "live" dot. `color` defaults to the ok/accent token at the callsite. */
export function LiveDot({
  color = "var(--ok)",
  size = "sm",
  fast = false,
}: {
  color?: string;
  size?: "sm" | "lg";
  fast?: boolean;
}) {
  return (
    <span
      className={`dot ${size === "lg" ? "lg" : ""} pulse ${fast ? "fast" : ""}`}
      style={{ background: color }}
    />
  );
}

/** Panel with a header row: title on the left, arbitrary nodes on the right. */
export function Panel({
  title,
  className = "",
  headRight,
  children,
}: {
  title: string;
  className?: string;
  headRight?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className={`panel ${className}`}>
      <div className="panel-head">
        <h2 className="panel-title">{title}</h2>
        {headRight}
      </div>
      {children}
    </section>
  );
}

export interface SegOption<T extends string> {
  value: T;
  label: ReactNode;
}

/** Generic segmented control. `variant="mode"` styles the online/offline pair. */
export function Segmented<T extends string>({
  options,
  value,
  onChange,
  variant,
  modeKind,
}: {
  options: SegOption<T>[];
  value: T;
  onChange: (v: T) => void;
  variant?: "mode";
  modeKind?: (v: T) => "on" | "off";
}) {
  return (
    <div className="seg">
      {options.map((o) => {
        const active = o.value === value;
        const kind = variant === "mode" && modeKind ? ` ${modeKind(o.value)}` : "";
        return (
          <button
            key={o.value}
            className={`seg-btn ${variant === "mode" ? "mode" : ""} ${active ? "active" : ""}${active ? kind : ""}`}
            onClick={() => onChange(o.value)}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

/** Inline ecosystem chip (colored square + uppercase eco name on a tint). */
export function EcoChip({
  eco,
  color,
  tint,
  border,
  style,
}: {
  eco: string;
  color: string;
  tint?: string;
  border?: string;
  style?: CSSProperties;
}) {
  return (
    <span
      className="eco-chip"
      style={{ color, background: tint, border: border ? `1px solid ${border}` : undefined, ...style }}
    >
      <span className="eco-square" style={{ background: color }} />
      {eco}
    </span>
  );
}
