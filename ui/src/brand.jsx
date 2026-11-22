import React from "react";
import fullMark from "../../brand/mark-themed.svg?raw";
import smallMark from "../../brand/mark-small-themed.svg?raw";

// The Super Cage mark (brand/design-language.md §5). The masters are imported
// straight from brand/, the single source, so the desktop and the phone (its
// Logo in mobile-cli/doodles.jsx draws this component) cannot drift apart. Its
// path data is large, so it is added to the document once as two <symbol>s and
// every use is a <use> reference. Ink follows currentColor; the light follows
// --clay and its glow --clay-soft.
const inner = (svg) =>
  svg
    .replace(/^[\s\S]*?<svg[^>]*>/, "")
    .replace(/<\/svg>\s*$/, "")
    .replace(/<title>[\s\S]*?<\/title>/, "");

if (typeof document !== "undefined" && !document.getElementById("sc-mark")) {
  document.body.insertAdjacentHTML(
    "afterbegin",
    '<svg xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false" style="position:absolute;width:0;height:0;overflow:hidden">' +
      '<symbol id="sc-mark" viewBox="0 0 256 256">' + inner(fullMark) + "</symbol>" +
      '<symbol id="sc-mark-small" viewBox="0 0 256 256">' + inner(smallMark) + "</symbol></svg>",
  );
}

// The small cut is drawn for 16–32 px; the full mark from about 40 px.
export function BrandMark({ size = 24, className = "", label }) {
  return (
    <svg
      className={"brand-mark-svg" + (className ? " " + className : "")}
      width={size}
      height={size}
      viewBox="0 0 256 256"
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      focusable="false"
    >
      <use href={size <= 32 ? "#sc-mark-small" : "#sc-mark"} />
    </svg>
  );
}

export function Wordmark({ className = "" }) {
  return <span className={"wordmark" + (className ? " " + className : "")}>Super Cage</span>;
}
