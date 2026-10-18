import { AudioLines, Boxes, Clapperboard, FlaskConical, Image, Library, MessageSquare, MonitorSmartphone, Music, ScrollText, SquareUserRound, Tags, Wrench } from "lucide-react";
import type { ComponentType } from "react";
import { t } from "@/lib/i18n";
import { cn } from "@/lib/utils";
import { Logo } from "./Logo";
import { navigate, useRoute } from "@/lib/route";
import { isActive, useApp } from "@/store/app";
import { useShallow } from "zustand/react/shallow";
import { initials } from "@/lib/devices";
import { rememberedDeviceName } from "@/lib/device";
import { useSession } from "@/store/session";

interface Item {
  page: string;
  label: string;
  icon: ComponentType<{ className?: string }>;
}

const groups: { title?: string; items: Item[] }[] = [
  {
    title: "Create",
    items: [
      { page: "chat", label: "Chat", icon: MessageSquare },
      { page: "image", label: "Image", icon: Image },
      { page: "video", label: "Video", icon: Clapperboard },
      { page: "music", label: "Music", icon: Music },
      { page: "voice", label: "Voice", icon: AudioLines },
      { page: "avatar", label: "Avatar", icon: SquareUserRound },
    ],
  },
  { items: [{ page: "library", label: "Library", icon: Library }] },
  {
    title: "Lab",
    items: [
      { page: "embed", label: "Embeddings", icon: FlaskConical },
      { page: "classify", label: "Classify", icon: Tags },
    ],
  },
  {
    title: "Settings",
    items: [
      { page: "models", label: "Models", icon: Boxes },
      { page: "prompts", label: "Prompts", icon: ScrollText },
      { page: "tools", label: "Tools", icon: Wrench },
      { page: "devices", label: "Devices", icon: MonitorSmartphone },
    ],
  },
];

/** Which pages make which kind (for the busy dots). */
const kindOf: Record<string, string> = { chat: "chat", image: "image", video: "video", music: "music", voice: "speech", avatar: "avatar", embed: "embed", classify: "classify" };
/** Pages that make more than one kind. */
const alsoKind: Record<string, string> = { voice: "sound" };

export function Sidebar() {
  const { page } = useRoute();
  const busy = new Set(useApp(useShallow((s) => Object.values(s.jobs).filter(isActive).map((j) => j.kind))));
  return (
    <nav className="flex h-full w-56 shrink-0 flex-col gap-5 overflow-y-auto border-r border-[var(--line)] bg-[var(--surface-2)] px-3 py-4 max-md:hidden">
      <a href="#/image" className="flex items-center gap-2.5 px-1.5">
        <Logo />
        <span className="font-semibold tracking-tight">mlxlab</span>
      </a>
      {groups.map((g, i) => (
        <div key={i} className="flex flex-col gap-0.5">
          {g.title && <div className="px-2.5 pb-1 text-[11px] font-semibold tracking-[.07em] text-muted-foreground uppercase">{t(g.title)}</div>}
          {g.items.map((it) => (
            <button
              key={it.page}
              onClick={() => navigate(it.page)}
              className={cn(
                "flex min-h-9 items-center gap-2.5 rounded-[10px] px-2.5 text-sm text-[var(--ink-2)] transition-colors hover:bg-sidebar-accent hover:text-sidebar-foreground",
                page === it.page && "bg-[var(--teal-soft)] font-medium text-[var(--teal-ink)] hover:bg-[var(--teal-soft)] hover:text-[var(--teal-ink)]",
              )}
            >
              <it.icon className="size-4" />
              <span className="flex-1 text-left">{t(it.label)}</span>
              {(busy.has(kindOf[it.page] as never) || busy.has(alsoKind[it.page] as never)) && <span className="dot live" style={{ "--c": "var(--teal)" } as React.CSSProperties} />}
            </button>
          ))}
        </div>
      ))}
      <SignedInAs />
    </nav>
  );
}

/** At the foot of the sidebar on the hub: who is signed in, on which device ("Philipp · Chrome on macOS"). */
function SignedInAs() {
  const user = useSession((s) => (s.info?.role === "hub" && s.info.signedIn ? (s.info.user ?? "") : null));
  const { page } = useRoute();
  if (user === null) return null;
  const device = rememberedDeviceName();
  return (
    <button
      onClick={() => navigate("devices")}
      title={`${user || t("No user name")} · ${device}`}
      className={cn(
        "mt-auto flex items-center gap-2.5 rounded-[10px] px-2 py-1.5 text-left transition-colors hover:bg-sidebar-accent",
        page === "devices" && "bg-sidebar-accent",
      )}
      data-signed-in-as=""
    >
      <span className="grid size-7 shrink-0 place-items-center rounded-full bg-primary/15 text-xs font-semibold text-[var(--teal-ink)]" aria-hidden>
        {initials(user)}
      </span>
      <span className="min-w-0 flex-1 leading-tight">
        <span className="block truncate text-sm font-medium text-sidebar-foreground">{user || t("(no name)")}</span>
        <span className="block truncate text-[11px] text-muted-foreground">{device}</span>
      </span>
    </button>
  );
}

/** The same places as a bar at the bottom, on a narrow screen. */
export function BottomNav() {
  const { page } = useRoute();
  const items = [...groups[0].items, groups[1].items[0]];
  return (
    <nav className="top-bar flex border-t border-b-0 pb-[env(safe-area-inset-bottom)] md:hidden">
      {items.map((it) => (
        <button
          key={it.page}
          onClick={() => navigate(it.page)}
          className={cn("flex min-h-14 flex-1 flex-col items-center justify-center gap-0.5 text-[11px] text-muted-foreground", page === it.page && "font-medium text-[var(--teal-ink)]")}
        >
          <it.icon className="size-5" />
          {t(it.label)}
        </button>
      ))}
    </nav>
  );
}
