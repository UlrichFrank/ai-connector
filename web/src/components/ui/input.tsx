import * as React from "react"
import { cn } from "cn"

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "h-10 w-full min-w-0 rounded-[12px] border border-[var(--line-2)] bg-[var(--surface)] px-3 py-1 text-base shadow-[var(--inset-track)] transition-[border-color,box-shadow] outline-none file:inline-flex file:h-6 file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-foreground placeholder:text-[var(--ink-4)] focus-visible:border-[var(--teal)] focus-visible:shadow-[0_0_0_4px_var(--focus)] disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 md:text-sm",
        className
      )}
      // prompts, lyrics and names are meant as typed: no autocorrect or spell-check marks (a prop overrides)
      spellCheck={false}
      autoCorrect="off"
      autoCapitalize="off"
      {...props}
    />
  )
}

export { Input }
