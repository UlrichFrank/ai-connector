import { Loader2, RotateCw } from "lucide-react";
import { Logo } from "@/components/shell/Logo";
import { Button } from "@/components/ui/button";
import { t } from "@/lib/i18n";
import { useSession } from "@/store/session";
import { Backdrop } from "./Backdrop";

/** While /api/info loads, and when it does not answer at all. */
export function Splash() {
  const phase = useSession((s) => s.phase);
  const boot = useSession((s) => s.boot);
  const unreachable = phase === "unreachable";
  return (
    <Backdrop>
      <div className="flex flex-col items-center gap-4 text-center">
        <Logo large />
        {unreachable ? (
          <>
            <div>
              <div className="font-semibold">{t("Can't reach mlxlab")}</div>
              <p className="mt-1 text-sm text-muted-foreground">{t("The server does not answer. Trying again in a moment …")}</p>
            </div>
            <Button variant="outline" onClick={() => boot()}>
              <RotateCw /> {t("Try now")}
            </Button>
          </>
        ) : (
          <Loader2 className="size-5 animate-spin text-muted-foreground" aria-label={t("Loading")} />
        )}
      </div>
    </Backdrop>
  );
}
