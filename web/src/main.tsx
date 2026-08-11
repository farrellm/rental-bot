import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

// Self-hosted, so the binary serves every byte the page needs. Only the
// subsets and weights the design uses are imported — everything here is
// embedded into the binary, so an unused subset is dead weight in the deploy.
import "@fontsource-variable/archivo/wght.css";
import "@fontsource/space-mono/latin-400.css";

import "./styles/tokens.css";
import "./styles/base.css";
import "./styles/card.css";
import "./styles/shell.css";
import "./styles/controls.css";
import "./styles/property.css";
import "./styles/tabs.css";
import "./styles/ledger.css";
import "./styles/docket.css";
import "./styles/lease.css";
import "./styles/jacket.css";
import "./styles/register.css";
import "./styles/intake.css";
import "./styles/dispatch.css";

import { AppRoutes } from "./routes";
import { ApiError } from "./api";

const client = new QueryClient({
  defaultOptions: {
    queries: {
      // A 401 is an answer, not a hiccup: retrying it three times only delays
      // sending the operator to sign in.
      retry: (failureCount, error) =>
        !(error instanceof ApiError && error.isUnauthenticated) && failureCount < 2,
      refetchOnWindowFocus: false,
    },
  },
});

const root = document.getElementById("root");
if (!root) throw new Error("index.html is missing #root");

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={client}>
      <BrowserRouter>
        <AppRoutes />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
