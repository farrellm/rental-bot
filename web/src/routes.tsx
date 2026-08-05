import { Navigate, Route, Routes } from "react-router";

import { AppShell } from "./AppShell";
import { ApiError } from "./api/client";
import { useMe } from "./api/queries";
import { Properties } from "./screens/Properties";
import { PropertyDetail } from "./screens/PropertyDetail";
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
        {/* "new" is a property that does not exist yet, on the same card. */}
        <Route path="/properties/:id" element={<PropertyDetail />} />
        <Route path="/service" element={<Service />} />
        <Route path="/" element={<Navigate to="/properties" replace />} />
      </Route>

      {/* The SPA fallback serves index.html for any path, so an unknown one
          lands here rather than at the server. */}
      <Route path="*" element={<Navigate to="/properties" replace />} />
    </Routes>
  );
}
