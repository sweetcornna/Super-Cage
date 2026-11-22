import { useMedia, TOUCH_NARROW } from "./touch-query";

export const useIconsOnly = () => useMedia(TOUCH_NARROW);

// A status bar item's words. On a narrow touch screen the bar shows icons only (touch.css): the words stay for
// screen readers and become the tooltip (a long press shows it, icon-hints.jsx).
export function StatusLabel({ children }) {
  return <span className={useIconsOnly() ? "visually-hidden" : undefined}>{children}</span>;
}
