import type { ReactNode } from "react";

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
