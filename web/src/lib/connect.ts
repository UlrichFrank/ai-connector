// "Connect a Mac" (docs/architecture.md, "Connecting a Mac from the web UI"): the one command to paste on the Mac.
// Pure (no imports): tested with `pnpm test` (node --test, src/lib/connect.test.ts).

/** Where the program goes on the Mac (and where `mlxlab worker` keeps its settings, one folder up). */
export const MAC_BIN = "~/.config/mlxlab/bin/mlxlab";

/** Single-quotes a shell word: nothing inside is special to sh/zsh/bash; a ' becomes '\'' (close, escaped, reopen). */
export function shq(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/** The hub's address as the worker should dial it: the origin, without a trailing slash. */
export function hubUrl(origin: string): string {
  return origin.trim().replace(/\/+$/, "");
}

/**
 * The command that downloads mlxlab from this hub into ~/.config/mlxlab/bin and starts it as a worker with its token.
 * ONE command over several lines: each line break is escaped ("\"), so a paste runs all of it, and every step only
 * runs if the one before worked. The token goes in the environment for the worker (never as its argument); the file
 * name of the download is `uname -s`-`uname -m` (Darwin-arm64), so another machine gets the hub's clear 404.
 */
export function connectCommand(origin: string, token: string): string {
  const hub = hubUrl(origin);
  const dir = MAC_BIN.slice(0, MAC_BIN.lastIndexOf("/"));
  return [
    `mkdir -p ${dir}`,
    `&& curl -fsSL -H ${shq(`Authorization: Bearer ${token}`)} -o ${MAC_BIN} ${shq(`${hub}/tools/mlxlab/`)}"$(uname -s)-$(uname -m)"`,
    `&& chmod +x ${MAC_BIN}`,
    `&& MLXLAB_WORKER_TOKEN=${shq(token)} ${MAC_BIN} worker --hub ${shq(hub)}`,
  ].join(" \\\n  ");
}

/** The command as shown on screen: the token replaced by its first characters and dots (it is copied, not read). */
export function maskToken(text: string, token: string): string {
  if (!token) return text;
  const keep = token.length > 12 ? token.slice(0, 4) : "";
  const mask = keep + "•".repeat(12);
  // as typed, and as it stands inside single quotes (a ' in it is written '\'')
  return text.split(token).join(mask).split(token.replace(/'/g, `'\\''`)).join(mask);
}
