"""Accounts: who exists, their role, and who a user reports to — plus the policy for
who may create, change, and delete whom.

Three roles: `user` < `admin` < `superuser`.
  * superuser — creates any role; sets any user's reports-to; promotes/demotes;
    deletes anyone. The env-superuser (settings.ROOT_USER) is an always-present,
    unmanageable superuser verified from the environment, never stored.
  * admin — creates `user` accounts that report to them, and manages (password
    reset, delete) only those reports. Cannot touch roles or other admins' users.
  * user — no account-management rights; may change only their own password.

A `user` always reports to an admin or superuser; admins and superusers report to
no one (reports_to is forced None for them). The reporting graph lives here; project
ownership does not — that is a separate registry the enforcement phase adds, so the
"can't delete/demote while owning projects" rule is out of this module's scope for
now (the "still has reports" rule below IS enforced, since it is knowable here).

Behaviour lives on the class; the store (load/save/LOCK) and the hasher are injected,
so the policy is testable without a real file or a real KDF."""
import re

from app.errors import ApiError, ForbiddenError

# Distinguishes "reports_to not supplied" from "reports_to set to null" in an update.
_UNSET = object()

ROLES = ("user", "admin", "superuser")
_MANAGERS = ("admin", "superuser")  # roles a user may report to

_MIN_PASSWORD = 8
# Lowercase alnum separated by single . _ or -, 1–40 chars — same DNS/path-safe
# grammar as project names, so a username is never ambiguous in a URL or a log.
_NAME_RE = re.compile(r"^[a-z0-9]+(?:[._-][a-z0-9]+)*$")


class AccountError(ApiError):
    """A bad account request (invalid name/role/password, duplicate, missing target).
    Maps to 400 via the ApiError contract; 404 for a missing target is passed
    explicitly."""


class Account:
    """A caller-facing view of an account — never carries the password hash. `builtin`
    marks the env-superuser, which the API surfaces but refuses to modify."""

    __slots__ = ("username", "role", "reports_to", "builtin")

    def __init__(self, username, role, reports_to=None, builtin=False):
        self.username = username
        self.role = role
        self.reports_to = reports_to
        self.builtin = builtin


class Accounts:
    def __init__(self, store, hasher, root_user=None, root_password=None):
        self._store = store
        self._hasher = hasher
        self._root_user = root_user
        self._root_password = root_password

    # ---- predicates ----------------------------------------------------------
    def enabled(self):
        """Whether auth enforcement is active: a root superuser is configured, or the
        store already holds accounts. When neither holds, the deployment has not opted
        into auth and the enforcement layer leaves the cache routes open (as before
        this feature) — so setting UI_ROOT_USER is what turns enforcement on."""
        if self._root_user:
            return True
        return bool(self._store.load()["users"])

    def can_operate(self, actor, owner):
        """Whether `actor` may run owner-level operations on a resource owned by
        `owner` (a username, or None for superuser-owned): a superuser always, else
        only the owner themselves."""
        if actor.role == "superuser":
            return True
        if owner is None:
            return False
        return actor.username == owner

    def can_view(self, actor, owner):
        """Whether `actor` may view/consume a resource owned by `owner`: anyone who can
        operate it, plus the owner's direct reports (a user sees their admin's
        projects)."""
        if self.can_operate(actor, owner):
            return True
        if owner is None:
            return False
        return actor.reports_to == owner

    def _is_root(self, username):
        return self._root_user is not None and username == self._root_user

    # ---- queries -------------------------------------------------------------
    def authenticate(self, username, password):
        """The Account for valid credentials, else None. Checks the env-superuser
        first (verified from the environment), then the stored users."""
        username = (username or "").strip()
        if self._is_root(username) and self._root_password is not None:
            if self._verify_root(password):
                return self._root_account()
            return None
        record = self._store.load()["users"].get(username)
        if record and self._hasher.verify(password or "", record.get("salt", ""), record.get("hash", "")):
            return self._map(username, record)
        return None

    def get(self, username):
        """The Account for a username (env-superuser or stored), or None. Used to
        resolve a live session back to its caller."""
        if self._is_root(username):
            return self._root_account()
        record = self._store.load()["users"].get(username)
        return self._map(username, record) if record else None

    def list(self, actor):
        """The accounts `actor` may see: a superuser sees everyone (env-superuser
        included); an admin sees themselves and their direct reports; a user sees only
        themselves."""
        users = self._store.load()["users"]
        if actor.role == "superuser":
            out = [self._map(name, rec) for name, rec in sorted(users.items())]
            if self._root_user:
                out.insert(0, self._root_account())
            return out
        if actor.role == "admin":
            reports = [self._map(name, rec) for name, rec in sorted(users.items())
                       if rec.get("reports_to") == actor.username]
            return [actor_view(actor), *reports]
        return [actor_view(actor)]

    # ---- creators ------------------------------------------------------------
    def create(self, actor, username, password, role, reports_to=None):
        """Create a stored account on `actor`'s authority. An admin may create only
        `user` accounts reporting to themselves; a superuser may create any role."""
        if actor.role not in _MANAGERS:
            raise ForbiddenError("only admins and superusers can create accounts")
        if role not in ROLES:
            raise AccountError(f"role must be one of {', '.join(ROLES)}")
        username = self._validate_name(username)
        self._validate_password(password)

        if actor.role == "admin":
            if role != "user":
                raise ForbiddenError("admins can only create users")
            reports_to = actor.username
        else:  # superuser
            reports_to = self._resolve_reports_to(role, reports_to, subject=username)

        with self._store.LOCK:
            data = self._store.load()
            if username in data["users"]:
                raise AccountError(f"account already exists: {username}")
            salt, digest = self._hasher.hash(password)
            data["users"][username] = {
                "role": role, "salt": salt, "hash": digest, "reports_to": reports_to,
            }
            self._store.save(data)
        return Account(username, role, reports_to)

    # ---- updaters ------------------------------------------------------------
    def update(self, actor, username, *, role=None, reports_to=_UNSET, password=None):
        """Change a stored account's role, manager, and/or password, enforced against
        `actor`'s authority and the reporting invariants."""
        if self._is_root(username):
            raise ForbiddenError("the root superuser is managed via the environment, not the API")
        with self._store.LOCK:
            data = self._store.load()
            record = data["users"].get(username)
            if record is None:
                raise AccountError(f"no such account: {username}", 404)

            if password is not None:
                self._authorize_password(actor, username, record)
                self._validate_password(password)
                record["salt"], record["hash"] = self._hasher.hash(password)

            role_change = role is not None and role != record["role"]
            reports_change = reports_to is not _UNSET
            if role_change or reports_change:
                if actor.role != "superuser":
                    raise ForbiddenError("only a superuser can change roles or reporting")
                if role_change and username == actor.username:
                    raise ForbiddenError("you cannot change your own role")
                if role_change and role not in ROLES:
                    raise AccountError(f"role must be one of {', '.join(ROLES)}")

            final_role = role if role_change else record["role"]
            final_reports = reports_to if reports_change else record.get("reports_to")
            final_reports = self._settle_reports(username, record, final_role, final_reports, data["users"])
            record["role"] = final_role
            record["reports_to"] = final_reports

            self._store.save(data)
            return Account(username, final_role, final_reports)

    # ---- deleters ------------------------------------------------------------
    def delete(self, actor, username):
        """Remove a stored account. A superuser deletes anyone (never the root or
        themselves, never an admin who still has reports); an admin deletes only their
        own `user` reports."""
        if self._is_root(username):
            raise ForbiddenError("the root superuser is managed via the environment, not the API")
        if username == actor.username:
            raise ForbiddenError("you cannot delete your own account")
        with self._store.LOCK:
            data = self._store.load()
            record = data["users"].get(username)
            if record is None:
                raise AccountError(f"no such account: {username}", 404)
            self._authorize_delete(actor, username, record, data["users"])
            del data["users"][username]
            self._store.save(data)

    # ---- private: authorization ----------------------------------------------
    def _authorize_password(self, actor, username, record):
        if actor.role == "superuser":
            return
        if actor.username == username:
            return
        if actor.role == "admin" and record.get("reports_to") == actor.username:
            return
        raise ForbiddenError("you may only change your own password or your reports'")

    def _authorize_delete(self, actor, username, record, users):
        if actor.role == "superuser":
            if self._has_reports(username, users):
                raise ForbiddenError("reassign this account's reports before deleting it")
            return
        if actor.role == "admin":
            if record.get("role") != "user" or record.get("reports_to") != actor.username:
                raise ForbiddenError("admins may only delete their own users")
            return
        raise ForbiddenError("users cannot delete accounts")

    # ---- private: reporting invariants ---------------------------------------
    def _resolve_reports_to(self, role, reports_to, *, subject):
        """The stored reports_to for a new account: a manager for a user (validated),
        None for an admin/superuser."""
        if role != "user":
            return None
        return self._validate_manager(reports_to, subject=subject)

    def _settle_reports(self, username, record, final_role, final_reports, users):
        """Reconcile reports_to with the (possibly changed) role: a user must have a
        valid manager; an admin/superuser has none. Blocks demoting an account that
        still has reports."""
        if final_role != "user":
            return None
        if record["role"] != "user" and self._has_reports(username, users):
            raise ForbiddenError("reassign this account's reports before demoting it")
        return self._validate_manager(final_reports, subject=username)

    def _validate_manager(self, manager, *, subject):
        if not manager:
            raise AccountError("a user must report to an admin or superuser")
        if manager == subject:
            raise AccountError("an account cannot report to itself")
        target = self.get(manager)
        if target is None or target.role not in _MANAGERS:
            raise AccountError(f"reports_to must be an existing admin or superuser: {manager}")
        return manager

    def _has_reports(self, username, users):
        return any(rec.get("reports_to") == username for rec in users.values())

    # ---- private: validation + mapping ---------------------------------------
    def _validate_name(self, name):
        name = (name or "").strip()
        if not (1 <= len(name) <= 40) or not _NAME_RE.fullmatch(name):
            raise AccountError(
                "username must be 1–40 chars, lowercase letters/digits separated by "
                "single '.', '_' or '-'"
            )
        if self._is_root(name):
            raise AccountError(f"'{name}' is reserved")
        return name

    def _validate_password(self, password):
        if not password or len(password) < _MIN_PASSWORD:
            raise AccountError(f"password must be at least {_MIN_PASSWORD} characters")

    def _verify_root(self, password):
        import hmac
        return hmac.compare_digest((password or "").encode(), self._root_password.encode())

    def _root_account(self):
        return Account(self._root_user, "superuser", None, builtin=True)

    def _map(self, username, record):
        return Account(username, record["role"], record.get("reports_to"))


def actor_view(actor):
    """The caller's own account as a plain Account (drops any builtin flag for the
    self entry in a listing)."""
    return Account(actor.username, actor.role, actor.reports_to, actor.builtin)
