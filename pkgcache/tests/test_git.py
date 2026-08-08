"""Focused request-boundary tests for the Git smart-HTTP handler."""

import gzip

import pytest

from pkgcache.handlers.git import _NegotiationTooLarge, _gunzip_limited


def test_gzip_negotiation_body_is_bounded_after_decompression():
    compressed = gzip.compress(b"x" * 1025)

    with pytest.raises(_NegotiationTooLarge):
        _gunzip_limited(compressed, limit=1024)


def test_gzip_negotiation_body_round_trips_within_limit():
    payload = b"0010git-protocol"

    assert _gunzip_limited(gzip.compress(payload), limit=1024) == payload
