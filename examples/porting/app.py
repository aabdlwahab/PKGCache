"""Proof that the dependencies installed through the cache actually work."""
import requests

print("requests", requests.__version__, "- installed through package-registry")
