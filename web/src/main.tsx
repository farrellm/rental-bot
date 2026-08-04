import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

// Self-hosted, so the binary serves every byte the page needs. Only the
// subsets and weights the design uses are imported — everything here is
// embedded into the binary, so an unused subset is dead weight in the deploy.
import "@fontsource-variable/archivo/wght.css";
import "@fontsource/space-mono/latin-400.css";

import "./styles/tokens.css";
import "./styles/base.css";
import "./styles/card.css";

import App from "./App";

const root = document.getElementById("root");
if (!root) throw new Error("index.html is missing #root");

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
