import { NavLink, Outlet, useNavigate } from "react-router";
import { useQueryClient } from "@tanstack/react-query";

import { signOut } from "./api/queries";

/**
 * The desk and the label on the drawer.
 *
 * The band sits on the ground, never on stock: it is the drawer these cards
 * came out of rather than one of the cards. Everything a screen renders below
 * it is a record.
 */
export function AppShell() {
  const navigate = useNavigate();
  const client = useQueryClient();

  async function handleSignOut() {
    try {
      await signOut();
    } catch {
      // A session that was already gone is a session that is gone. Either way
      // the cookies are cleared and there is nothing left to hold.
    }
    client.clear();
    void navigate("/sign-in", { replace: true });
  }

  return (
    <div className="shell">
      <header className="shell__band">
        <NavLink to="/properties" className="shell__mark">
          rental-bot
        </NavLink>
        <nav className="shell__nav">
          <NavLink to="/properties" className="shell__link">
            Properties
          </NavLink>
          <NavLink to="/service" className="shell__link">
            Service
          </NavLink>
          <button type="button" className="shell__link" onClick={() => void handleSignOut()}>
            Sign out
          </button>
        </nav>
      </header>

      <Outlet />
    </div>
  );
}
