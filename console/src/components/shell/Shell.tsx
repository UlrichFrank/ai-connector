import { Bot, Boxes, ExternalLink, KeyRound, LayoutDashboard, LogOut, Menu, Plug, Server } from "lucide-react";
import { useState, type ComponentType, type ReactNode } from "react";
import { Dot } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetTitle } from "@/components/ui/sheet";
import { isActive } from "@/lib/jobs";
import { navigate, useRoute } from "@/lib/route";
import { LANGS, plural, setLang, t, useLang } from "@/lib/i18n";
import { cn } from "@/lib/utils";
import { useLive } from "@/store/live";
import { useSession } from "@/store/session";

/** The mark: a plug on the Home OS icon chip (teal). */
export function Logo({ className, large }: { className?: string; large?: boolean }) {
  return (
    <span aria-hidden className={cn("ico", large ? "lg" : "sm", className)}>
      <Plug />
    </span>
  );
}

interface Item {
  page: string;
  label: string;
  icon: ComponentType<{ className?: string }>;
}

const navItems: Item[] = [
  { page: "overview", label: "Overview", icon: LayoutDashboard },
  { page: "workers", label: "Workers", icon: Server },
  { page: "models", label: "Models", icon: Boxes },
  { page: "clients", label: "Clients", icon: KeyRound },
  { page: "connectors", label: "Connectors", icon: Bot },
];

function Nav({ onPick }: { onPick?: () => void }) {
  const { page: route } = useRoute();
  const page = route === "link" ? "clients" : route; // linking a device makes a client
  const busy = useLive((s) => Object.values(s.jobs).some(isActive));
  const workers = useLive((s) => s.connected.length);
  return (
    <div className="flex flex-col gap-0.5">
      {navItems.map((it) => (
        <button
          key={it.page}
          onClick={() => {
            navigate(it.page);
            onPick?.();
          }}
          aria-current={page === it.page ? "page" : undefined}
          className={cn(
            "flex min-h-10 items-center gap-2.5 rounded-[10px] px-2.5 text-sm text-[var(--ink-2)] transition-colors hover:bg-sidebar-accent hover:text-sidebar-foreground",
            page === it.page && "bg-[var(--teal-soft)] font-medium text-[var(--teal-ink)] hover:bg-[var(--teal-soft)] hover:text-[var(--teal-ink)]",
          )}
        >
          <it.icon className="size-4" />
          <span className="flex-1 text-left">{t(it.label)}</span>
          {it.page === "overview" && busy && <span className="dot live" style={{ "--c": "var(--teal)" } as React.CSSProperties} />}
          {it.page === "workers" && <span className="text-xs text-muted-foreground tabular-nums">{workers}</span>}
        </button>
      ))}
    </div>
  );
}

/** DE / EN, kept per browser (home.lang) like the house's other web UIs. */
export function LangSwitch({ className }: { className?: string }) {
  const lang = useLang();
  return (
    <div className={cn("seg", className)} role="group" aria-label={t("Language")}>
      {LANGS.map((l) => (
        <button key={l} type="button" aria-pressed={lang === l} onClick={() => setLang(l)}>
          {l.toUpperCase()}
        </button>
      ))}
    </div>
  );
}

function Foot() {
  const user = useSession((s) => s.info?.user ?? "");
  const hub = useSession((s) => s.info?.role === "hub");
  const signOut = useSession((s) => s.signOut);
  return (
    <div className="mt-auto flex flex-col gap-2">
      <a href="/" className="flex min-h-10 items-center gap-2.5 rounded-[10px] px-2.5 text-sm text-[var(--ink-2)] hover:bg-sidebar-accent hover:text-sidebar-foreground">
        <ExternalLink className="size-4" /> {t("mlxlab app")}
      </a>
      {hub && (
        <div className="flex items-center gap-2 px-1">
          <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">{user ? t("Signed in as {user}", { user }) : t("Signed in")}</span>
          <Button variant="ghost" size="icon-sm" title={t("Sign out")} onClick={() => void signOut()}>
            <LogOut />
            <span className="sr-only">{t("Sign out")}</span>
          </Button>
        </div>
      )}
    </div>
  );
}

function Brand() {
  return (
    <a href="#/overview" className="flex items-center gap-2.5 px-1.5">
      <Logo />
      <span className="font-semibold tracking-tight">ai-connector</span>
    </a>
  );
}

/** The sidebar on a wide screen; a sheet behind the menu button on a narrow one. */
export function Sidebar() {
  return (
    <nav className="flex h-full w-60 shrink-0 flex-col gap-5 border-r border-[var(--line)] bg-[var(--surface-2)] px-3 py-4 max-md:hidden">
      <Brand />
      <Nav />
      <Foot />
    </nav>
  );
}

/** The job stream's state, for the top bar. */
function LiveBadge() {
  const connection = useLive((s) => s.connection);
  const n = useLive((s) => s.connected.length);
  const tone = connection === "live" ? "ok" : connection === "offline" ? "bad" : "busy";
  const text =
    connection === "live" ? plural(n, "{n} worker · live", "{n} workers · live") : connection === "waiting" ? t("no worker connected") : connection === "offline" ? t("reconnecting …") : t("connecting …");
  return (
    <span className="flex h-9 items-center gap-2 rounded-full border border-[var(--line-2)] bg-[var(--surface)] px-3 text-xs font-medium whitespace-nowrap text-[var(--ink-2)] shadow-[var(--elevation-1),var(--highlight)] max-sm:px-2.5" data-connection={connection}>
      <Dot tone={tone} pulse={connection !== "live"} /> {text}
    </span>
  );
}

export function TopBar({ title }: { title: string }) {
  const [open, setOpen] = useState(false);
  return (
    <header className="top-bar flex min-h-14 shrink-0 items-center gap-2 px-4 pt-[env(safe-area-inset-top)] max-sm:px-3">
      <Button variant="ghost" size="icon" className="size-11 md:hidden" onClick={() => setOpen(true)} aria-label={t("Menu")}>
        <Menu />
      </Button>
      <h1 className="min-w-0 truncate text-[15px] font-semibold tracking-tight max-sm:sr-only">{title}</h1>
      <a href="#/overview" className="sm:hidden" aria-label="ai-connector">
        <Logo />
      </a>
      <div className="flex-1" />
      <LiveBadge />
      <LangSwitch />
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="left" className="w-64 gap-5 bg-[var(--surface-2)] px-3 py-4 pt-[max(1rem,env(safe-area-inset-top))]">
          <SheetTitle className="sr-only">{t("Navigation")}</SheetTitle>
          <Brand />
          <Nav onPick={() => setOpen(false)} />
          <Foot />
        </SheetContent>
      </Sheet>
    </header>
  );
}

/** The full-screen stage of the sign-in and loading screens: a soft teal glow behind a centred column. */
export function Backdrop({ children }: { children: ReactNode }) {
  return (
    <div className="relative flex min-h-full items-center justify-center overflow-hidden px-4 py-10">
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 bg-[radial-gradient(60rem_32rem_at_50%_-10%,color-mix(in_oklch,var(--teal)_12%,transparent),transparent_70%)]"
      />
      <div className="relative w-full max-w-sm">{children}</div>
    </div>
  );
}
