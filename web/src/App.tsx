import { useEffect, type ReactNode } from "react";
import { PlayerBar } from "@/components/kit/PlayerBar";
import { ChatPage } from "@/features/chat";
import { ImagePage } from "@/features/image";
import { ClassifyPage, EmbedPage } from "@/features/lab";
import { LibraryPage } from "@/features/library";
import { MusicPage } from "@/features/music";
import { ModelsPage, PromptsPage } from "@/features/settings";
import { ToolsPage } from "@/features/settings/ToolsPage";
import { VideoPage } from "@/features/video";
import { VoicePage } from "@/features/voice";
import { AvatarPage } from "@/features/avatar";
import { DevicesPage } from "@/components/devices/DevicesPage";
import { SignIn } from "@/components/session/SignIn";
import { Splash } from "@/components/session/Splash";
import { BottomNav, Sidebar } from "@/components/shell/Sidebar";
import { TopBar } from "@/components/shell/TopBar";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { t, useLang } from "@/lib/i18n";
import { useRoute } from "@/lib/route";
import { useDoneToasts } from "@/lib/notify";
import { useSession } from "@/store/session";

const pages: Record<string, { title: string; render: (arg: string) => ReactNode }> = {
  chat: { title: "Chat", render: (arg) => <ChatPage arg={arg} /> },
  image: { title: "Image", render: (arg) => <ImagePage arg={arg} /> },
  video: { title: "Video", render: (arg) => <VideoPage arg={arg} /> },
  music: { title: "Music", render: (arg) => <MusicPage arg={arg} /> },
  voice: { title: "Voice", render: (arg) => <VoicePage arg={arg} /> },
  avatar: { title: "Avatar", render: (arg) => <AvatarPage arg={arg} /> },
  library: { title: "Library", render: (arg) => <LibraryPage arg={arg} /> },
  embed: { title: "Embeddings", render: (arg) => <EmbedPage arg={arg} /> },
  classify: { title: "Classify", render: (arg) => <ClassifyPage arg={arg} /> },
  models: { title: "Models", render: (arg) => <ModelsPage arg={arg} /> },
  prompts: { title: "Prompts", render: (arg) => <PromptsPage arg={arg} /> },
  tools: { title: "Tools", render: (arg) => <ToolsPage arg={arg} /> },
  devices: { title: "Devices", render: () => <DevicesPage /> },
};

/** Signs in first (on the hub), then the app. */
export default function App() {
  const phase = useSession((s) => s.phase);
  const boot = useSession((s) => s.boot);
  // a new language renders everything again (the key), the stores keep their data
  const lang = useLang();
  useEffect(() => void boot(), [boot]);
  return (
    <TooltipProvider delayDuration={300} key={lang}>
      {phase === "ready" ? <Shell /> : phase === "signin" ? <SignIn /> : <Splash />}
      {/* on a phone, above the bottom nav */}
      <Toaster position="bottom-right" mobileOffset={{ bottom: 72 }} />
    </TooltipProvider>
  );
}

function Shell() {
  useDoneToasts();
  const { page, arg } = useRoute();
  const p = pages[page] ?? pages.image;
  return (
    <div className="flex h-full">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar title={t(p.title)} />
        <main className="min-h-0 flex-1 overflow-y-auto">{p.render(arg)}</main>
        <PlayerBar />
        <BottomNav />
      </div>
    </div>
  );
}
