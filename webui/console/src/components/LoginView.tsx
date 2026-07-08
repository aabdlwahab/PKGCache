import { useState } from "react";

// The full-screen sign-in gate shown when auth is enabled and there is no session.
// Nothing else of the app renders until login succeeds.
export function LoginView({
  onLogin,
}: {
  onLogin: (username: string, password: string) => Promise<void>;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || !username || !password) return;
    setBusy(true);
    setError(null);
    try {
      await onLogin(username, password);
    } catch (err) {
      setError((err as Error).message);
      setBusy(false); // stay on the form; on success the app unmounts this view
    }
  };

  return (
    <div className="login-screen">
      <form className="login-card" onSubmit={submit}>
        <div className="brand" style={{ justifyContent: "center", marginBottom: "1.25rem" }}>
          <span className="wordmark">
            <span className="br">[</span>
            <span className="f">pkg</span>
            <span className="a">cache</span>
            <span className="br">]</span>
          </span>
        </div>
        <label className="field-label" htmlFor="login-user">
          username
        </label>
        <input
          id="login-user"
          className="input"
          autoFocus
          autoComplete="username"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
        />
        <label className="field-label" htmlFor="login-pass" style={{ marginTop: "0.75rem" }}>
          password
        </label>
        <input
          id="login-pass"
          className="input"
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        {error && (
          <div className="login-error" role="alert">
            {error}
          </div>
        )}
        <button
          className="btn btn-primary"
          type="submit"
          disabled={busy || !username || !password}
          style={{ marginTop: "1.1rem", width: "100%", justifyContent: "center" }}
        >
          {busy ? "signing in…" : "sign in"}
        </button>
      </form>
    </div>
  );
}
