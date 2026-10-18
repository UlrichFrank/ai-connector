import { LANGS, setLang, t, useLang } from "@/lib/i18n";
import { cn } from "@/lib/utils";

/** The mark: an "m" on the Home OS icon chip (teal). */
export function Logo({ className, large }: { className?: string; large?: boolean }) {
  return (
    <span aria-hidden className={cn("ico font-bold", large ? "lg text-2xl" : "sm text-sm", className)}>
      m
    </span>
  );
}

/** DE / EN, kept per browser (home.lang) like the house's other web UIs. */
export function LangSwitch({ className }: { className?: string }) {
  const lang = useLang();
  return (
    <div className={cn("seg shrink-0", className)} role="group" aria-label={t("Language")}>
      {LANGS.map((l) => (
        <button key={l} type="button" aria-pressed={lang === l} onClick={() => setLang(l)}>
          {l.toUpperCase()}
        </button>
      ))}
    </div>
  );
}
