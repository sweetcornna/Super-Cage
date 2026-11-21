import React from "react";

// Claude mark: Simple Icons, CC0-1.0 (the path of claude-logo.svg), drawn in
// currentColor so a row stays quiet; clay is kept for live state.
const claude = "m4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z";
// Codex: braces around a point, filled like a brush stroke (thin at the ends,
// full along the long strokes, pinched at the point), so it carries the same
// weight as the Claude mark beside it. Drawn for Super Cage from centre lines;
// it is not OpenAI's logo.
const codex = "M10.1 2.6L9.2 2.6L8.3 2.7L7.5 3.1L6.7 3.6L6.2 4.4L5.8 5.3L5.6 6.2L5.6 7.1L5.6 7.9L5.6 8.6L5.5 9.3L5.3 10L4.7 10.7L3.5 12L4.7 13.3L5.3 14L5.5 14.7L5.6 15.4L5.6 16.1L5.6 16.9L5.6 17.8L5.8 18.7L6.2 19.6L6.7 20.4L7.5 20.9L8.3 21.3L9.2 21.4L10.1 21.4A0.68 0.68 0 0 1 10.3 20L10.3 20L9.5 19.9L9 19.6L8.7 19.3L8.6 19L8.5 18.6L8.5 18.2L8.6 17.6L8.6 16.7L8.4 15.7L8.1 14.7L7.5 13.7L6.7 12.8L5.7 12L4.9 12L5.7 12L6.7 11.2L7.5 10.3L8.1 9.3L8.4 8.3L8.6 7.3L8.6 6.4L8.5 5.8L8.5 5.4L8.6 5L8.7 4.7L9 4.4L9.5 4.1L10.3 4A0.68 0.68 0 0 1 10.1 2.6ZM13.7 4L14.5 4.1L15 4.4L15.3 4.7L15.4 5L15.5 5.4L15.5 5.8L15.4 6.4L15.4 7.3L15.6 8.3L15.9 9.3L16.5 10.3L17.3 11.2L18.3 12L19.1 12L18.3 12L17.3 12.8L16.5 13.7L15.9 14.7L15.6 15.7L15.4 16.7L15.4 17.6L15.5 18.2L15.5 18.6L15.4 19L15.3 19.3L15 19.6L14.5 19.9L13.7 20A0.68 0.68 0 0 1 13.9 21.4L13.9 21.4L14.8 21.4L15.7 21.3L16.5 20.9L17.3 20.4L17.8 19.6L18.2 18.7L18.4 17.8L18.4 16.9L18.4 16.1L18.4 15.4L18.5 14.7L18.7 14L19.3 13.3L20.5 12L19.3 10.7L18.7 10L18.5 9.3L18.4 8.6L18.4 7.9L18.4 7.1L18.4 6.2L18.2 5.3L17.8 4.4L17.3 3.6L16.5 3.1L15.7 2.7L14.8 2.6L13.9 2.6A0.68 0.68 0 0 1 13.7 4ZM12 10.2a1.8 1.8 0 1 1 0 3.6a1.8 1.8 0 1 1 0-3.6Z";

const names = { claude: "Claude", codex: "Codex" };

// The provider slot at the start of a session row: a fixed square, so a row
// lays out the same whichever provider fills it. One prop picks the provider.
// An account number, when there is one, sits in a small pill on the corner,
// outside the layout box. It is drawn by CSS from data-number, so it stays
// crisp text at 1x and never joins the row's textContent. The slot is an <i>,
// not a span: row code and tests take a row's first span as its title.
export function ProviderMark({ provider = "claude", accountNumber, size = 16, className = "", decorative = false }) {
  const number = Number.isInteger(accountNumber) && accountNumber > 0 ? String(accountNumber) : "";
  const label = (names[provider] || provider) + (number ? ` · 账号 ${number}` : "");
  return (
    <i className={`provider-mark provider-mark-${provider}${className ? " " + className : ""}`}
      style={{ width: size, height: size }} data-number={number || undefined}
      role={decorative ? undefined : "img"} aria-label={decorative ? undefined : label} aria-hidden={decorative || undefined}>
      <svg width={size} height={size} viewBox="0 0 24 24" aria-hidden="true" focusable="false">
        <path d={provider === "codex" ? codex : claude} fill="currentColor" />
      </svg>
    </i>
  );
}
