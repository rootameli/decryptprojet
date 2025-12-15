#!/usr/bin/env python3
"""Generate candidate SMTP hostnames for a given domain.

The script is intentionally lightweight: it derives common subdomains from the
input domain (e.g., smtp.example.com, mail.example.com) and prints one hostname
per line. Workers pick up these candidates during the TLS hostname recovery
flow in `test-smtps`.
"""

import sys
from itertools import product

DEFAULT_HOSTS = [
    "archivio",
    "autodiscover",
    "carddav",
    "in",
    "out",
    "syncdav",
    "webmail",
]

COMMON_PREFIXES = [
    "smtp",
    "smtp1",
    "smtp2",
    "mail",
    "mail1",
    "mail2",
    "mx",
    "relay",
    "news",
    "notif",
]

COMMON_SUFFIXES = ["", "-backup", "-edge"]


def build_candidates(domain: str) -> list[str]:
    domain = domain.strip()
    if not domain:
        return []
    candidates: list[str] = []
    seen: set[str] = set()

    # Include the bare domain first.
    for host in [domain] + [f"{p}.{domain}" for p in COMMON_PREFIXES]:
        if host not in seen:
            seen.add(host)
            candidates.append(host)

    # Add prefixed variants with suffixes.
    for prefix, suffix in product(COMMON_PREFIXES, COMMON_SUFFIXES):
        host = f"{prefix}{suffix}.{domain}"
        if host not in seen:
            seen.add(host)
            candidates.append(host)

    for h in DEFAULT_HOSTS:
        host = f"{h}.{domain}"
        if host not in seen:
            seen.add(host)
            candidates.append(host)

    return candidates


def main(argv: list[str]) -> int:
    if len(argv) < 2:
        sys.stderr.write("usage: sub.py <domain>\n")
        return 1
    domain = argv[1]
    hosts = build_candidates(domain)
    for host in hosts:
        print(host)
    try:
        output_file = f"{domain}.txt"
        with open(output_file, "w", encoding="utf-8") as f:
            for host in hosts:
                f.write(f"{host}\n")
    except OSError:
        # If writing fails, we still return success so stdout is usable.
        pass
    return 0


if __name__ == "__main__":  # pragma: no cover - thin wrapper
    raise SystemExit(main(sys.argv))
