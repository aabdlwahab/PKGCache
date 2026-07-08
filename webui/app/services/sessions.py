"""Session store: opaque login tokens → usernames, held in memory, plus a per-IP
failed-login throttle.

State, not workflow — so it owns its own dict and a lock (the account server is
multi-threaded). Tokens are unguessable and server-side, so logout/revocation is a
delete and a webui restart clears every session. Expiry and the lockout window use a
monotonic clock so a wall-clock adjustment can't extend or void a session."""
import secrets
import threading
import time


class Sessions:
    def __init__(self, ttl, *, max_failures=5, lockout=300):
        self._ttl = ttl
        self._max_failures = max_failures
        self._lockout = lockout
        self._tokens = {}       # token -> (username, expiry_monotonic)
        self._failures = {}     # ip -> (count, lock_until_monotonic)
        self._lock = threading.Lock()

    # ---- sessions ------------------------------------------------------------
    def create(self, username):
        """Mint a session for `username` and return its opaque token."""
        token = secrets.token_urlsafe(32)
        with self._lock:
            self._tokens[token] = (username, time.monotonic() + self._ttl)
        return token

    def resolve(self, token):
        """The username a live token maps to, or None (expired tokens are dropped)."""
        if not token:
            return None
        with self._lock:
            entry = self._tokens.get(token)
            if entry is None:
                return None
            username, expiry = entry
            if time.monotonic() >= expiry:
                del self._tokens[token]
                return None
            return username

    def drop(self, token):
        """Revoke a token (logout). A no-op if it is unknown or already expired."""
        with self._lock:
            self._tokens.pop(token, None)

    # ---- login throttle ------------------------------------------------------
    def blocked(self, ip):
        """Whether `ip` is currently locked out after too many failed logins."""
        with self._lock:
            entry = self._failures.get(ip)
            return entry is not None and time.monotonic() < entry[1]

    def record_failure(self, ip):
        """Count a failed login from `ip`; lock it out once the threshold is hit."""
        with self._lock:
            count = self._failures.get(ip, (0, 0.0))[0] + 1
            lock_until = time.monotonic() + self._lockout if count >= self._max_failures else 0.0
            self._failures[ip] = (count, lock_until)

    def clear_failures(self, ip):
        """Reset the throttle for `ip` after a successful login."""
        with self._lock:
            self._failures.pop(ip, None)
