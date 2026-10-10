/**
 * Copies inside the click: first the synchronous way (a selected textarea and execCommand), which also works on plain
 * http (a hub in the LAN) where the async Clipboard API is missing; then the Clipboard API.
 */
export async function copyText(text: string): Promise<void> {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.cssText = "position:fixed;top:0;left:0;opacity:0";
  // inside the open dialog: Radix keeps the focus there, a textarea outside could not be selected
  (document.activeElement?.closest("[role=dialog]") ?? document.body).appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  }
  ta.remove();
  if (ok) return;
  if (navigator.clipboard?.writeText) return navigator.clipboard.writeText(text);
  throw new Error("This browser does not let the page copy. Select the text and copy it yourself.");
}
