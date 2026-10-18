import { CircleAlert, Info, Loader2, LockKeyhole } from "lucide-react";
import { useState, type FormEvent } from "react";
import { RateLimitError, SignInError, errorText } from "@/api/client";
import { LangSwitch, Logo } from "@/components/shell/Logo";
import { t } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { local } from "@/lib/storage";
import { deviceName, rememberDeviceName, rememberedDeviceName } from "@/lib/device";
import { useSession } from "@/store/session";
import { Backdrop } from "./Backdrop";

/** The hub's sign-in: one password, and a name for this browser in the device list. */
export function SignIn() {
  const note = useSession((s) => s.note);
  const signIn = useSession((s) => s.signIn);
  const [password, setPassword] = useState("");
  const [user, setUser] = useState(() => local.get("user", ""));
  const [device, setDevice] = useState(rememberedDeviceName);
  const [naming, setNaming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!password || !user.trim() || busy) return;
    setBusy(true);
    setError("");
    try {
      const name = device.trim() || deviceName();
      await signIn(password, name, user.trim());
      rememberDeviceName(name);
      local.set("user", user.trim());
    } catch (err) {
      setError(message(err));
      setPassword("");
      setBusy(false);
    }
  };

  return (
    <Backdrop>
      <div className="mb-8 flex flex-col items-center gap-3 text-center">
        <Logo large />
        <div>
          <h1 className="text-2xl font-semibold tracking-[-0.03em]">mlxlab</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t("Sign in to use the models on your Mac.")}</p>
        </div>
      </div>

      <form onSubmit={submit} className="card p-5 sm:p-6">
        {note && !error && (
          <div className="mb-4 flex items-start gap-2 rounded-lg bg-muted px-3 py-2 text-sm text-muted-foreground">
            <Info className="mt-0.5 size-4 shrink-0" />
            {note}
          </div>
        )}
        <div className="mb-4 grid gap-2">
          <Label htmlFor="user">{t("Your name")}</Label>
          <Input id="user" name="username" autoComplete="username" placeholder={t("e.g. Philipp")} maxLength={40} value={user} onChange={(e) => setUser(e.target.value)} autoFocus={!user} />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="password">{t("Password")}</Label>
          <div className="relative">
            <LockKeyhole className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              id="password"
              type="password"
              autoComplete="current-password"
              autoFocus={!!user}
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              aria-invalid={!!error || undefined}
              aria-describedby={error ? "signin-error" : undefined}
              className="pl-9"
            />
          </div>
        </div>

        <div aria-live="polite">
          {error && (
            <p id="signin-error" className="mt-2.5 flex items-start gap-1.5 text-sm text-destructive">
              <CircleAlert className="mt-0.5 size-4 shrink-0" />
              {error}
            </p>
          )}
        </div>

        <Button type="submit" size="lg" className="mt-5 w-full" disabled={!password || !user.trim() || busy}>
          {busy && <Loader2 className="animate-spin" />}
          {busy ? t("Signing in …") : t("Sign in")}
        </Button>

        <div className="mt-4 border-t pt-4 text-xs text-muted-foreground">
          {naming ? (
            <div className="grid gap-1.5">
              <Label htmlFor="device" className="text-xs font-normal text-muted-foreground">
                {t("Name of this device in the device list")}
              </Label>
              <Input id="device" value={device} maxLength={60} onChange={(e) => setDevice(e.target.value)} onBlur={() => setNaming(false)} autoFocus />
            </div>
          ) : (
            <div className="flex items-center justify-between gap-2">
              <span className="truncate">
                {t("Signing in as")} <span className="font-medium text-foreground">{device.trim() || deviceName()}</span>
              </span>
              <button type="button" className="-my-2 min-h-11 shrink-0 font-medium text-[var(--teal-ink)] hover:underline" onClick={() => setNaming(true)}>
                {t("Rename")}
              </button>
            </div>
          )}
        </div>
      </form>
      <div className="mt-6 flex justify-center">
        <LangSwitch />
      </div>
    </Backdrop>
  );
}

function message(e: unknown): string {
  if (e instanceof SignInError) return t("That password is not right.");
  if (e instanceof RateLimitError)
    return e.retryAfter
      ? e.retryAfter < 90
        ? t("Too many attempts. Try again in {n} seconds.", { n: e.retryAfter })
        : t("Too many attempts. Try again in {n} minutes.", { n: Math.ceil(e.retryAfter / 60) })
      : t("Too many attempts. Wait a minute and try again.");
  return errorText(e);
}
