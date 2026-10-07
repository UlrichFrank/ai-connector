/** A name for this browser, for the device list: "Safari on macOS", "Chrome on iPhone", "Firefox on Windows". */
export function deviceName(ua = navigator.userAgent): string {
  return `${browser(ua)} on ${system(ua)}`;
}

function browser(ua: string): string {
  if (/Edg(e|A|iOS)?\//.test(ua)) return "Edge";
  if (/OPR\/|Opera/.test(ua)) return "Opera";
  if (/Firefox\/|FxiOS\//.test(ua)) return "Firefox";
  if (/Chrome\/|CriOS\//.test(ua)) return "Chrome";
  if (/Safari\//.test(ua)) return "Safari";
  return "Browser";
}

function system(ua: string): string {
  if (/iPhone/.test(ua)) return "iPhone";
  // iPadOS asks for the desktop site and looks like a Mac, except for its touch screen
  if (/iPad/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1)) return "iPad";
  if (/Android/.test(ua)) return "Android";
  if (/Mac OS X|Macintosh/.test(ua)) return "macOS";
  if (/Windows/.test(ua)) return "Windows";
  if (/CrOS/.test(ua)) return "ChromeOS";
  if (/Linux/.test(ua)) return "Linux";
  return "this device";
}

const key = "mlxlab.device";

/** The name this browser signed in with last time (when it was renamed), else one from the user agent. */
export function rememberedDeviceName(): string {
  try {
    return localStorage.getItem(key) || deviceName();
  } catch {
    return deviceName();
  }
}

export function rememberDeviceName(name: string) {
  try {
    if (name === deviceName()) localStorage.removeItem(key);
    else localStorage.setItem(key, name);
  } catch {
    // private mode: the default next time
  }
}
