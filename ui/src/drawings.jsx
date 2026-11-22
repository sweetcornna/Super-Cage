import React from "react";
import { Doodle } from "./mobile-cli/doodles.jsx";
import { brushArt } from "./drawings-art.mjs";

// Hand-drawn art for the desktop (brand/design-language.md §4), in the brush
// hand of the mark. Ink follows currentColor; clay follows --clay. The phone
// keeps its own line drawings in mobile-cli/doodles.jsx; anything not redrawn
// here falls back to those.
export function Drawing({ name, className = "", label }) {
  const art = brushArt[name];
  const classes = "drawing drawing-" + name + (className ? " " + className : "");
  if (!art) return <Doodle name={name} className={classes} label={label} />;
  return (
    <span
      className={classes}
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      dangerouslySetInnerHTML={{ __html: art }}
    />
  );
}

// brand/design-language.md §4: one drawing, a serif heading, one line and at
// most one action.
export function EmptyState({ drawing, title, children, action, compact = false, className = "" }) {
  return (
    <div className={"empty-state" + (compact ? " compact" : "") + (className ? " " + className : "")}>
      {drawing && <Drawing name={drawing} />}
      <h3>{title}</h3>
      {children && <p>{children}</p>}
      {action && <div className="empty-action">{action}</div>}
    </div>
  );
}
