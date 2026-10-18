// "Connect a worker": the one paste that downloads the worker program from this server and starts it with its token.
// Pure (no imports): tested with `pnpm test` (node --test, src/lib/install.test.ts).

/** Where the program goes on macOS and Linux (`ai-connector worker` keeps its settings one folder up). */
export const UNIX_BIN = "~/.config/mlxlab/bin/ai-connector";
/** Where it goes on Windows (PowerShell). */
export const WINDOWS_DIR = "$env:LOCALAPPDATA\\ai-connector";
export const WINDOWS_EXE = `${WINDOWS_DIR}\\ai-connector.exe`;

export type InstallOS = "unix" | "windows";

/** Single-quotes a shell word: nothing inside is special to sh/zsh/bash; a ' becomes '\'' (close, escaped, reopen). */
export function shq(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/** Single-quotes a PowerShell string: nothing inside is expanded; a ' (or a typographic one) is doubled. */
export function psq(s: string): string {
  return `'${s.replace(/['\u2018\u2019\u201a\u201b]/g, (q) => q + q)}'`;
}

/** The server's address as a worker dials it and a client calls it: the origin, without a trailing slash. */
export function serverUrl(origin: string): string {
  return origin.trim().replace(/\/+$/, "");
}

/**
 * macOS and Linux: ONE command over several lines (each line break escaped, so a paste runs all of it, and each step
 * only if the one before worked). The download's name is `uname -s`-`uname -m` (Darwin-arm64, Linux-x86_64,
 * Linux-aarch64): the server maps it to its build, or answers a clear 404. The token goes to the worker in its
 * environment, never as an argument.
 */
export function unixInstallCommand(origin: string, token: string): string {
  const server = serverUrl(origin);
  const dir = UNIX_BIN.slice(0, UNIX_BIN.lastIndexOf("/"));
  return [
    `mkdir -p ${dir}`,
    `&& curl -fsSL -H ${shq(`Authorization: Bearer ${token}`)} -o ${UNIX_BIN} ${shq(`${server}/tools/mlxlab/`)}"$(uname -s)-$(uname -m)"`,
    `&& chmod +x ${UNIX_BIN}`,
    `&& MLXLAB_WORKER_TOKEN=${shq(token)} ${UNIX_BIN} worker --hub ${shq(server)}`,
  ].join(" \\\n  ");
}

/**
 * Windows PowerShell (5.1 and 7): one script block, so a paste runs as a whole and stops at the first error
 * ($ErrorActionPreference = 'Stop'), without leaving variables behind in the session except the worker's token.
 */
export function windowsInstallCommand(origin: string, token: string): string {
  const server = serverUrl(origin);
  return [
    "& {",
    "  $ErrorActionPreference = 'Stop'",
    `  $dir = Join-Path $env:LOCALAPPDATA 'ai-connector'`,
    "  New-Item -ItemType Directory -Force -Path $dir | Out-Null",
    `  $exe = Join-Path $dir 'ai-connector.exe'`,
    `  Invoke-WebRequest -UseBasicParsing -Headers @{ Authorization = ${psq(`Bearer ${token}`)} } -OutFile $exe -Uri ${psq(`${server}/tools/mlxlab/windows-amd64`)}`,
    `  $env:MLXLAB_WORKER_TOKEN = ${psq(token)}`,
    `  & $exe worker --hub ${psq(server)}`,
    "}",
  ].join("\n");
}

export function installCommand(os: InstallOS, origin: string, token: string): string {
  return os === "windows" ? windowsInstallCommand(origin, token) : unixInstallCommand(origin, token);
}

/** How to start the worker again later (it remembers the server and the token). */
export function restartCommand(os: InstallOS): string {
  return os === "windows" ? `& "${WINDOWS_EXE}" worker` : `${UNIX_BIN} worker`;
}

/**
 * How to start it at every login (and now): a launchd agent, a systemd user unit or a scheduled task, with the server
 * and the token from the file the first start kept (never on the service's command line). `--uninstall` undoes it.
 */
export function loginCommand(os: InstallOS): string {
  return `${restartCommand(os)} --install`;
}

/** The OS of this browser, for the tab the dialog opens on. */
export function guessOS(ua: string): InstallOS {
  return /Windows/.test(ua) ? "windows" : "unix";
}

/** The text as shown on screen: the token replaced by its first characters and dots (it is copied, not read). */
export function maskToken(text: string, token: string): string {
  if (!token) return text;
  const keep = token.length > 12 ? token.slice(0, 4) : "";
  const mask = keep + "•".repeat(12);
  // as typed, and as it stands inside sh or PowerShell single quotes
  return text.split(token).join(mask).split(token.replace(/'/g, `'\\''`)).join(mask).split(token.replace(/'/g, "''")).join(mask);
}
