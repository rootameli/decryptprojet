#!/usr/bin/env python3
import sys

def generate(domain: str):
    base = domain.strip()
    if not base:
        return []
    prefixes = ["smtp", "mail", "mx", "relay"]
    return [f"{p}.{base}" for p in prefixes]

if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(0)
    for host in generate(sys.argv[1]):
        print(host)
