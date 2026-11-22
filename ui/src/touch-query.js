import { useSyncExternalStore } from "react";

// The workbench under a finger (touch.css): a coarse pointer that cannot hover, and below a width where the
// status bar has no room for its words.
export const TOUCH = "(pointer: coarse) and (hover: none)";
export const TOUCH_NARROW = `${TOUCH} and (max-width: 900px)`;

export function useMedia(query) {
  return useSyncExternalStore((notify) => {
    const media = matchMedia(query);
    media.addEventListener("change", notify);
    return () => media.removeEventListener("change", notify);
  }, () => matchMedia(query).matches);
}
