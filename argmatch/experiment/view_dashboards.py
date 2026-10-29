#!/usr/bin/env python3
"""Simulate a person viewing prombench's Grafana dashboards, so that Grafana
queries prometheus-meta as it did in the run whose stack log argmatch uses.

Lists the dashboards through Grafana's search API, then loads them one after
another in headless Chromium (Playwright), each for --dwell seconds with the
last hour as the time range, cycling until --duration seconds have passed.
Prints the number of dashboards and of panel query requests (/api/ds/query)
and their failures, so a viewer that isn't really querying is noticed.

Usage: view_dashboards.py --url http://localhost:3000/grafana [--duration 150] [--dwell 15]
Needs: pip3 install --user playwright && python3 -m playwright install --with-deps chromium
"""

import argparse
import json
import sys
import time
import urllib.request

from playwright.sync_api import sync_playwright


def main():
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--url", required=True, help="Grafana's base URL, including its sub path")
    p.add_argument("--duration", type=float, default=150, help="seconds to keep viewing")
    p.add_argument("--dwell", type=float, default=15, help="seconds to stay on each dashboard")
    args = p.parse_args()
    base = args.url.rstrip("/")
    end = time.time() + args.duration

    with urllib.request.urlopen(f"{base}/api/search?type=dash-db") as r:
        dashboards = [d for d in json.load(r) if d.get("uid")]
    if not dashboards:
        sys.exit("view_dashboards.py: no dashboards found")
    print(f"view_dashboards.py: {len(dashboards)} dashboards: {', '.join(d['title'] for d in dashboards)}", flush=True)

    counts = {"queries": 0, "failed": 0, "visits": 0}

    def on_response(resp):
        if "/api/ds/query" in resp.url:
            counts["queries"] += 1
            if resp.status >= 400:
                counts["failed"] += 1

    with sync_playwright() as pw:
        browser = pw.chromium.launch()
        page = browser.new_page(viewport={"width": 1600, "height": 2000})
        page.on("response", on_response)
        i = 0
        while time.time() < end:
            d = dashboards[i % len(dashboards)]
            i += 1
            try:
                page.goto(f"{base}/d/{d['uid']}?orgId=1&from=now-1h&to=now", wait_until="domcontentloaded", timeout=30000)
                counts["visits"] += 1
            except Exception as e:  # e.g. a timeout; keep going
                print(f"view_dashboards.py: {d['title']}: {e}", flush=True)
            page.wait_for_timeout(max(0, min(args.dwell, end - time.time())) * 1000)
        browser.close()
    print(f"view_dashboards.py: {counts['visits']} dashboard visits, {counts['queries']} panel queries, "
          f"{counts['failed']} failed", flush=True)


if __name__ == "__main__":
    main()
