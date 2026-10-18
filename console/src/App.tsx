import { useEffect, type ReactNode } from "react";
import { SignIn, Splash } from "@/components/shell/Session";
import { Sidebar, TopBar } from "@/components/shell/Shell";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { t, useLang } from "@/lib/i18n";
import { useRoute } from "@/lib/route";
import { ClientsPage } from "@/pages/Clients";
import { ConnectorsPage } from "@/pages/Connectors";
import { LinkPage } from "@/pages/Link";
import { ModelsPage } from "@/pages/Models";
import { OverviewPage } from "@/pages/Overview";
import { WorkersPage } from "@/pages/Workers";
import { useSession } from "@/store/session";

const pages: Record<string, { title: string; render: () => ReactNode }> = {
  overview: { title: "Overview", render: () => <OverviewPage /> },
  workers: { title: "Workers", render: () => <WorkersPage /> },
  models: { title: "Models", render: () => <ModelsPage /> },
  clients: { title: "Clients", render: () => <ClientsPage /> },
  connectors: { title: "Connectors", render: () => <ConnectorsPage /> },
  link: { title: "Link a device", render: () => <LinkPage /> }, // <hub>/pin leads here; not in the sidebar
};

/** Signs in first (on the server), then the console. */
export default function App() {
  const phase = useSession((s) => s.phase);
  const boot = useSession((s) => s.boot);
  // a new language renders everything again (the key), the stores keep their data
  const lang = useLang();
  useEffect(() => void boot(), [boot]);
  return (
    <TooltipProvider delayDuration={300} key={lang}>
      {phase === "ready" ? <Shell /> : phase === "signin" ? <SignIn /> : <Splash />}
      <Toaster position="bottom-right" />
    </TooltipProvider>
  );
}

function Shell() {
  const { page } = useRoute();
  const p = pages[page] ?? pages.overview;
  const title = t(p.title);
  useEffect(() => {
    document.title = `${title} · ai-connector`;
  }, [title]);
  return (
    <div className="flex h-full">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar title={title} />
        <main className="min-h-0 flex-1 overflow-y-auto">{p.render()}</main>
      </div>
    </div>
  );
}
