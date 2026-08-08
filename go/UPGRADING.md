# Upgrading an existing pkgreg instance

For an instance deployed before the P0 security pass and the guest-browsing change.

Every claim here was rehearsed against a simulated pre-upgrade deployment: a data
directory with populated databases, no accounts, and a configuration file predating
the new keys.

---

## The short version

**There is no data migration.** No schema changed in either database. The upgrade is a
binary swap, and rolling back is putting the old binary back.

**Swapping the binary and restarting `serve` changes almost nothing.** Authentication
stays off, the data plane is untouched, existing clients keep working.

**Running `pkgreg init` turns authentication on** — immediately, for the already
running process. That is the one step that changes who can use your instance, and on a
systemd install it happens automatically on the next restart. Read §2 before you
restart the service.

---

## 1. What is safe

Rehearsed against the old data directory with the new binary:

| | |
|---|---|
| Old `pkgreg.yaml` parses unchanged | Only new keys were added, and absent keys take their defaults |
| Catalog and control schema | Unchanged — no migration, no rewrite, no downtime |
| Cached content, blobs, snapshots | Untouched |
| Data plane: npm, PyPI, OCI, apt, git, files | Unaffected, including with authentication on |
| `/healthz`, `/readyz` | Unaffected, including over plain HTTP |
| `pkgreg-client` default shell | Works against an authenticated instance |
| `pkgreg-client -docker-trust` | Works against an authenticated instance |
| Existing project tokens | Unaffected — data-plane auth is per-project and independent |
| Rollback | Put the old binary back. No data to undo |

## 2. The one thing that will surprise you

`pkgreg init` provisions a superuser on any instance that has no accounts, and
**authentication is enforced the moment that account exists**. `Accounts.Enabled()`
reads the database live, so a *running* server starts refusing anonymous control-plane
calls without being restarted.

The systemd unit runs `ExecStartPre=pkgreg init -data-dir …` on every start. So on a
systemd install, `systemctl restart pkgreg` after the upgrade will:

1. provision an `admin` account,
2. print its password **once**, to the journal,
3. and start enforcing authentication on the control plane.

If that is what you want, good — that was the point of the change. If you are not ready
for it, see §5.

Recover the generated password from the journal:

```sh
journalctl -u pkgreg --since "10 minutes ago" | grep -A6 "CONTROL-PLANE LOGIN"
```

It is printed once and only its scrypt digest is stored. If you lose it, delete the row
and re-run `init`:

```sh
sqlite3 /var/lib/pkgreg/db/control.db "DELETE FROM users WHERE username='admin';"
pkgreg init -data-dir /var/lib/pkgreg      # prints a fresh password
```

> On a `DynamicUser=yes` systemd install the real path is
> `/var/lib/private/pkgreg/db/control.db`, reachable as root only.

## 3. What breaks, and what to do

### 3.1 Plain HTTP to the admin surface now redirects

On a single port that terminates TLS, origin-form plain HTTP used to serve the console,
metrics and the whole control API. It now answers `308` to the `https://` equivalent.
`/healthz` still answers in the clear, so liveness probes are unaffected.

Anything scripted against `http://host:8443/api/...` needs `https://`, or `curl -L`.
Check for it before you upgrade:

```sh
grep -rn "http://.*:8443" /etc /opt /srv --include='*.sh' --include='*.yml' 2>/dev/null
```

### 3.2 `pkgreg doctor` exit codes changed

It used to exit 0 on a default install. It now exits **4** on a non-loopback listener
with authentication off or an empty `server.proxy_allowlist`, and **3** on an
uninitialized data directory. Monitoring or CI that asserts `doctor` exits 0 will start
failing — which is the intended signal, but not on your schedule. Either fix the
posture (§4) or update the check first.

Also: `doctor` no longer creates anything. Pointed at a path that does not exist it
reports and stops instead of building a data directory there.

### 3.3 `pkgreg-client --persist` on an authenticated instance

The setup script requires a caller once authentication is on. The client now falls back
to the read-only guest session automatically, so `--persist` keeps working — provided
`auth.guest_read` is on, which it is by default.

If you set `guest_read: false`, `--persist` needs a session:

```
pkgreg-client: download setup script: server requires authentication: 401
This instance requires a signed-in caller and guest browsing is off.
Sign in to the console, then pass that session with -cookie-file.
```

### 3.4 `pkgreg <cmd> -h` now exits 0

It used to print usage and exit 1. Only relevant if something depended on the failure.

## 4. Recommended sequence

Rehearse on a copy first if the instance matters. There is no schema change, so a copy
of the data directory is a complete rehearsal environment.

```sh
# 0. Back up. Cheap, and the whole recovery story.
systemctl stop pkgreg
tar -C /var/lib -czf /root/pkgreg-backup-$(date +%F).tgz pkgreg
cp /usr/local/bin/pkgreg /root/pkgreg.previous       # the rollback artifact

# 1. Swap the binary.
install -m 0755 ./pkgreg-linux-amd64 /usr/local/bin/pkgreg
pkgreg version

# 2. Look before you leap: read-only, and it will not modify the data directory.
pkgreg doctor -config /var/lib/pkgreg/pkgreg.yaml || true

# 3. Close the open relay BEFORE restarting, so the first start is already clean.
#    List the repositories you actually mirror.
$EDITOR /var/lib/pkgreg/pkgreg.yaml
#   server:
#     proxy_allowlist:
#       - archive.ubuntu.com
#       - deb.debian.org
#       - "*.alpinelinux.org"
#    Use ["*"] to keep relaying anywhere as a deliberate, auditable choice.

# 4. Restart. On systemd this also runs init, which provisions the admin account
#    and turns authentication on. Capture the password immediately.
systemctl start pkgreg
journalctl -u pkgreg -n 60 | grep -A6 "CONTROL-PLANE LOGIN"

# 5. Confirm.
pkgreg doctor -config /var/lib/pkgreg/pkgreg.yaml; echo "exit=$?"   # want 0
curl -fsS --cacert /var/lib/pkgreg/certs/ca.crt https://HOST:8443/readyz
```

Then tell your developers two things: the console now asks for a login, and there is a
**Browse as guest** button that gives read-only access to the global project without an
account.

## 5. Staying on the old posture

If you need the upgrade without the behaviour change — say, to decouple a binary
rollout from an access-control change:

```yaml
auth:
  guest_read: false          # no sign-in-free browsing
server:
  proxy_allowlist: ["*"]     # keep relaying anywhere, deliberately
```

and **do not run `pkgreg init`**. On systemd that means editing the unit to drop
`ExecStartPre`, since it runs `init` on every start:

```sh
systemctl edit pkgreg      # add:  [Service] \n ExecStartPre=
```

`doctor` will still exit 4, correctly: the instance is still an unauthenticated control
plane on a reachable interface. This is a way to defer the change, not to make it safe.

## 6. Rolling back

No schema changed, so this is genuinely just the binary:

```sh
systemctl stop pkgreg
install -m 0755 /root/pkgreg.previous /usr/local/bin/pkgreg
systemctl start pkgreg
```

The `admin` account created by the new `init` is harmless to the old binary — it reads
the same `users` table and will simply enforce authentication against it. To return to
a fully open control plane, delete the row as in §2.

New keys left in `pkgreg.yaml` (`guest_read`, `proxy_allowlist`) **will fail to parse
under the old binary**, which rejects unknown keys by design. Remove them, or restore
the configuration file from the backup in §4.

## 7. Multi-instance and air-gapped sites

- **Peers/federation.** No wire-format change; a new instance and an old one interoperate.
  Upgrade in any order.
- **Air-gap packs.** The pack format is unchanged. Packs exported before the upgrade
  import after it, and vice versa.
- **CA and trust.** Untouched. `init` reuses an existing CA, so distributed trust and
  every configured client stay valid. No developer needs to re-run anything.
