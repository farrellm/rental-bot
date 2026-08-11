import { Navigate, Route, Routes } from "react-router";

import { AppShell } from "./AppShell";
import { ApiError, useMe } from "./api";
import { Properties } from "./screens/Properties";
import { PropertyRecord } from "./screens/PropertyRecord";
import { CashFlow } from "./screens/property/CashFlow";
import { Overview } from "./screens/property/Overview";
import { Documents } from "./screens/property/Documents";
import { Leases } from "./screens/property/Leases";
import { Repairs } from "./screens/property/Repairs";
import { Intake } from "./screens/Intake";
import { Service } from "./screens/Service";
import { SignIn } from "./screens/SignIn";

/**
 * Guards everything behind the session.
 *
 * The API answers 401 rather than redirecting, so the decision is made here:
 * ask who we are, and send anyone the server does not recognise to the slip.
 * While the answer is outstanding the screen says nothing at all — a flash of
 * the sign-in form on every reload would be a lie about being signed out.
 */
function RequireSession() {
  const me = useMe();

  if (me.isPending) {
    return <div className="shell" />;
  }
  if (me.isError) {
    const unauthenticated = me.error instanceof ApiError && me.error.isUnauthenticated;
    if (unauthenticated) {
      return <Navigate to="/sign-in" replace />;
    }
    // The server is reachable but unwell. Saying so beats a sign-in form that
    // will not work either.
    return (
      <div className="shell">
        <main className="shell__main shell__main--single shell__main--slip">
          <p className="empty__line">{me.error.message}</p>
        </main>
      </div>
    );
  }

  return <AppShell />;
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/sign-in" element={<SignIn />} />

      <Route element={<RequireSession />}>
        <Route path="/properties" element={<Properties />} />
        {/* A property that does not exist yet has nothing to hang tabs off,
            so it gets the Overview card alone. */}
        <Route path="/properties/new" element={<PropertyRecord isNew />} />
        {/* Sections are their own routes, so a link can name one. M4's review
            deep links will want exactly that. */}
        <Route path="/properties/:id" element={<PropertyRecord />}>
          <Route index element={<Overview />} />
          <Route path="cash-flow" element={<CashFlow />} />
          <Route path="repairs" element={<Repairs />} />
          <Route path="leases" element={<Leases />} />
          <Route path="documents" element={<Documents />} />
        </Route>
        <Route path="/intake" element={<Intake />} />
        <Route path="/service" element={<Service />} />
        <Route path="/" element={<Navigate to="/properties" replace />} />
      </Route>

      {/* The SPA fallback serves index.html for any path, so an unknown one
          lands here rather than at the server. */}
      <Route path="*" element={<Navigate to="/properties" replace />} />
    </Routes>
  );
}
