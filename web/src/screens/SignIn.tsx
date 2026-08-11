import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router";
import { useQueryClient } from "@tanstack/react-query";

import { ApiError, describeError, keys, signIn } from "../api";
import { Stamp } from "../components/Stamp";

/**
 * The admittance slip.
 *
 * A narrower card than a property record, because it holds two entries and a
 * word. A refusal stamps REFUSED across it and says nothing about which half
 * was wrong: the form is not an oracle for which accounts exist.
 */
export function SignIn() {
  const navigate = useNavigate();
  const client = useQueryClient();

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [working, setWorking] = useState(false);

  async function handleSubmit(event: FormEvent) {
    event.preventDefault();
    if (working) return;

    setWorking(true);
    setError(null);
    try {
      const user = await signIn(username, password);
      client.setQueryData(keys.me, user);
      void navigate("/properties", { replace: true });
    } catch (err) {
      setError(describeError(err));
      // Keep the username: retyping it is not what went wrong.
      setPassword("");
      if (err instanceof ApiError && err.status === 0) {
        setError("No contact with the server. The process may be stopped.");
      }
    } finally {
      setWorking(false);
    }
  }

  return (
    <main className="shell__main shell__main--single shell__main--slip">
      <form className="card card--slip" onSubmit={(e) => void handleSubmit(e)}>
        <header className="card__head">
          <div>
            <h1 className="card__mark stamped">rental-bot</h1>
            <p className="card__origin mono">{window.location.host}</p>
          </div>
        </header>

        {error && <p className="card__notice">{error}</p>}

        <div className="card__fields">
          <div className="field field--entry">
            <label className="field__label stamped" htmlFor="username">
              Username
            </label>
            <div className="field__value">
              <input
                id="username"
                name="username"
                className="entry"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                autoComplete="username"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                required
              />
            </div>
          </div>

          <div className="field field--entry">
            <label className="field__label stamped" htmlFor="password">
              Password
            </label>
            <div className="field__value">
              <input
                id="password"
                name="password"
                type="password"
                className="entry"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
                required
              />
            </div>
          </div>
        </div>

        <footer className="card__foot">
          <button type="submit" className="button button--primary" disabled={working}>
            {working ? "Signing in" : "Sign in"}
          </button>
          {error && <Stamp state="refused" />}
        </footer>
      </form>
    </main>
  );
}
