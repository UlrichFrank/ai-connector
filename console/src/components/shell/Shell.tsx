import { Bot, Boxes, ExternalLink, KeyRound, LayoutDashboard, LogOut, Menu, Monitor, Moon, Plug, Server, Sun } from "lucide-react";
import { useState, type ComponentType, type ReactNode } from "react";
import { Dot } from "@/components/common";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetTitle } from "@/components/ui/sheet";
import { isActive } from "@/lib/jobs";
import { navigate, useRoute } from "@/lib/route";
import { setTheme, useTheme, type Theme } from "@/lib/theme";
import { cn } from "@/lib/utils";
import { useLive } from "@/store/live";
import { useSession } from "@/store/session";

/** The mark: a violet tile with a plug. */
export function Logo({ className }: { className?: string }) {
  return (
    <span aria-hidden className={cn("grid size-7 shrink-0 place-items-center rounded-lg bg-primary text-primary-foreground", className)}>
      <Plug className="size-[60%]" />
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
  const { page } = useRoute();
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
            "flex items-center gap-2.5 rounded-md px-2 py-1.5 text-sm text-sidebar-foreground/80 transition-colors hover:bg-sidebar-accent hover:text-sidebar-foreground",
            page === it.page && "bg-sidebar-accent font-medium text-sidebar-foreground",
          )}
        >
          <it.icon className="size-4" />
          <span className="flex-1 text-left">{it.label}</span>
          {it.page === "overview" && busy && <span className="size-1.5 animate-pulse rounded-full bg-primary" />}
          {it.page === "workers" && <span className="text-xs text-muted-foreground tabular-nums">{workers}</span>}
        </button>
      ))}
    </div>
  );
}

function ThemeSwitch() {
  const theme = useTheme();
  const opts: { t: Theme; icon: ComponentType<{ className?: string }>; label: string }[] = [
    { t: "system", icon: Monitor, label: "System" },
    { t: "light", icon: Sun, label: "Light" },
    { t: "dark", icon: Moon, label: "Dark" },
  ];
  return (
    <div role="radiogroup" aria-label="Theme" className="flex rounded-md bg-sidebar-accent/60 p-0.5">
      {opts.map((o) => (
        <button
          key={o.t}
          role="radio"
          aria-checked={theme === o.t}
          title={o.label}
          onClick={() => setTheme(o.t)}
          className={cn("grid flex-1 place-items-center rounded py-1 text-muted-foreground transition-colors hover:text-foreground", theme === o.t && "bg-background text-foreground shadow-sm")}
        >
          <o.icon className="size-3.5" />
          <span className="sr-only">{o.label}</span>
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
      <a href="/" className="flex items-center gap-2 rounded-md px-2 py-1.5 text-sm text-sidebar-foreground/80 hover:bg-sidebar-accent hover:text-sidebar-foreground">
        <ExternalLink className="size-4" /> mlxlab app
      </a>
      <ThemeSwitch />
      {hub && (
        <div className="flex items-center gap-2 px-1">
          <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">{user ? `Signed in as ${user}` : "Signed in"}</span>
          <Button variant="ghost" size="icon-xs" title="Sign out" onClick={() => void signOut()}>
            <LogOut />
            <span className="sr-only">Sign out</span>
          </Button>
        </div>
      )}
    </div>
  );
}

function Brand() {
  return (
    <a href="#/overview" className="flex items-center gap-2 px-2">
      <Logo />
      <span className="font-semibold tracking-tight">ai-connector</span>
    </a>
  );
}

/** The sidebar on a wide screen; a sheet behind the menu button on a narrow one. */
export function Sidebar() {
  return (
    <nav className="flex h-full w-56 shrink-0 flex-col gap-5 border-r bg-sidebar px-3 py-4 max-md:hidden">
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
  const text = connection === "live" ? `${n} worker${n === 1 ? "" : "s"} · live` : connection === "waiting" ? "no worker connected" : connection === "offline" ? "reconnecting …" : "connecting …";
  return (
    <span className="flex items-center gap-1.5 rounded-md bg-muted/60 px-2 py-1 text-xs text-muted-foreground" data-connection={connection}>
      <Dot tone={tone} pulse={connection !== "live"} /> {text}
    </span>
  );
}

export function TopBar({ title }: { title: string }) {
  const [open, setOpen] = useState(false);
  return (
    <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4 max-sm:px-3">
      <Button variant="ghost" size="icon-sm" className="md:hidden" onClick={() => setOpen(true)} aria-label="Menu">
        <Menu />
      </Button>
      <h1 className="truncate text-sm font-semibold">{title}</h1>
      <div className="flex-1" />
      <LiveBadge />
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent side="left" className="w-64 gap-5 bg-sidebar px-3 py-4">
          <SheetTitle className="sr-only">Navigation</SheetTitle>
          <Brand />
          <Nav onPick={() => setOpen(false)} />
          <Foot />
        </SheetContent>
      </Sheet>
    </header>
  );
}

/** The full-screen stage of the sign-in and loading screens: a soft violet glow behind a centred column. */
export function Backdrop({ children }: { children: ReactNode }) {
  return (
    <div className="relative flex min-h-full items-center justify-center overflow-hidden px-4 py-10">
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 bg-[radial-gradient(60rem_32rem_at_50%_-10%,color-mix(in_oklch,var(--primary)_18%,transparent),transparent_70%)]"
      />
      <div className="relative w-full max-w-sm">{children}</div>
    </div>
  );
}
