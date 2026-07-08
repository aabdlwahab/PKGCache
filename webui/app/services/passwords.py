"""Password hashing — scrypt from the standard library, no third-party crypto dep
(the whole backend is deliberately stdlib-only for air-gapped deploys).

A fresh random salt per password; verification is constant-time. Injected into the
accounts service so the hashing choice is swappable and testable in isolation."""
import hashlib
import hmac
import secrets

# scrypt work factors: N=2^14 keeps a single hash well under ~100ms while making
# offline cracking expensive. maxmem is sized for 128*N*r bytes (~16 MiB) with head-
# room, since the OpenSSL default rejects N this large.
_N = 2 ** 14
_R = 8
_P = 1
_DKLEN = 32
_SALT_BYTES = 16
_MAXMEM = 64 * 1024 * 1024


class PasswordHasher:
    def hash(self, password):
        """(salt_hex, hash_hex) for a new password. Both stored; neither is secret on
        its own — only the password recreates the hash."""
        salt = secrets.token_bytes(_SALT_BYTES)
        return salt.hex(), self._derive(password, salt).hex()

    def verify(self, password, salt_hex, hash_hex):
        """True iff `password` reproduces the stored hash. Constant-time; a malformed
        stored value verifies to False rather than raising."""
        try:
            salt = bytes.fromhex(salt_hex)
            expected = bytes.fromhex(hash_hex)
        except (ValueError, TypeError):
            return False
        return hmac.compare_digest(self._derive(password, salt), expected)

    def _derive(self, password, salt):
        return hashlib.scrypt(
            password.encode("utf-8"), salt=salt, n=_N, r=_R, p=_P, dklen=_DKLEN, maxmem=_MAXMEM
        )
