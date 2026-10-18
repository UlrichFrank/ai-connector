import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import App from "./App.tsx";

// dark or light, as the system is (index.html sets it before the first paint): html.dark and html[data-theme]
const dark = matchMedia("(prefers-color-scheme: dark)");
// in the Wall Display panel (html[data-embed=wall-display]) always dark: it draws a dark gradient behind us
const embedded = () => document.documentElement.dataset.embed === "wall-display";
const theme = () => {
  const on = embedded() || dark.matches;
  document.documentElement.classList.toggle("dark", on);
  document.documentElement.dataset.theme = on ? "dark" : "light";
};
theme();
dark.addEventListener("change", theme);

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
