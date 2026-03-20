#!/usr/bin/env python3
"""
Scraper Worker — Finds Shopify sites via Tempest (Bing) and submits them
to the Site Manager API for checking.

Environment variables:
  CHECKER_URL  — Base URL of the checker service (e.g. https://checker.railway.internal:8080)
  API_KEY      — Shared secret for authentication (optional)
"""

import os
import re
import sys
import time
import random
import requests
import urllib3

urllib3.disable_warnings()

# ── Config ──────────────────────────────────────────────────────────
CHECKER_URL = os.environ.get("CHECKER_URL", "http://localhost:8080")
API_KEY = os.environ.get("API_KEY", "")

# Tempest (Bing-backed) search API
TEMPEST_BASE = "https://search-api.global.tempest.com"
TEMPEST_V1_SEARCH = f"{TEMPEST_BASE}/v1/search/"

# Country filter (set to None for worldwide)
TEMPEST_COUNTRY = os.environ.get("TEMPEST_COUNTRY", "US")
if TEMPEST_COUNTRY == "":
    TEMPEST_COUNTRY = None

IPAD_UA = (
    "Mozilla/5.0 (iPad; U; CPU OS 3_2_1 like Mac OS X; en-us) "
    "AppleWebKit/531.21.10 (KHTML, like Gecko) Mobile/7B405"
)

# How many sites to buffer before sending to the API
BATCH_SIZE = int(os.environ.get("SCRAPER_BATCH_SIZE", "50"))

# How many search iterations per cycle
SEARCHES_PER_CYCLE = int(os.environ.get("SCRAPER_SEARCHES", "500"))

# Delay between cycles (seconds)
CYCLE_DELAY = int(os.environ.get("SCRAPER_CYCLE_DELAY", "30"))

# ── Dorks ───────────────────────────────────────────────────────────
DORKS = [
    'site:myshopify.com',
    'site:myshopify.com store',
    'site:myshopify.com shop',
    'site:myshopify.com buy',
    'site:myshopify.com products',
    'site:myshopify.com collection',
    'site:myshopify.com cart',
    'site:myshopify.com checkout',
    'site:myshopify.com new',
    'site:myshopify.com sale',
    'site:myshopify.com deals',
    'site:myshopify.com best seller',
    'site:myshopify.com trending',
    'site:myshopify.com popular',
    'site:myshopify.com gift',
    'site:myshopify.com bundle',
    'site:myshopify.com makeup',
    'site:myshopify.com cosmetics',
    'site:myshopify.com beauty',
    'site:myshopify.com skincare',
    'site:myshopify.com clothing',
    'site:myshopify.com shoes',
    'site:myshopify.com jewelry',
    'site:myshopify.com accessories',
    'site:myshopify.com electronics',
    'site:myshopify.com fitness',
    'site:myshopify.com pet',
    'site:myshopify.com home decor',
    'site:myshopify.com furniture',
    'site:myshopify.com vitamins',
    'site:myshopify.com supplements',
    'site:myshopify.com organic',
    'site:myshopify.com handmade',
    'site:myshopify.com vintage',
    'site:myshopify.com luxury',
    'site:myshopify.com fashion',
    'site:myshopify.com kids',
    'site:myshopify.com baby',
    'site:myshopify.com outdoor',
    'site:myshopify.com sports',
    'site:myshopify.com tech',
    'site:myshopify.com gadgets',
    'site:myshopify.com art',
    'site:myshopify.com candles',
    'site:myshopify.com coffee',
    'site:myshopify.com tea',
    'site:myshopify.com food',
    'site:myshopify.com snacks',
]

# ── URL extraction ──────────────────────────────────────────────────
MYSHOPIFY_RE = re.compile(r"https?://([a-z0-9\-]+)\.myshopify\.com", re.IGNORECASE)


def normalize_url(raw: str) -> str | None:
    """Normalize a myshopify URL to https://<store>.myshopify.com"""
    m = MYSHOPIFY_RE.search(raw)
    if not m:
        return None
    store = m.group(1).lower().strip("-")
    if len(store) < 3:
        return None
    return f"https://{store}.myshopify.com"


def extract_shopify_urls(text: str) -> set[str]:
    """Extract all unique myshopify.com URLs from text."""
    urls = set()
    for m in MYSHOPIFY_RE.finditer(text):
        normalized = normalize_url(m.group(0))
        if normalized:
            urls.add(normalized)
    return urls


# ── API client ──────────────────────────────────────────────────────
def submit_sites(urls: list[str]) -> dict:
    """POST discovered sites to the checker API."""
    if not urls:
        return {"added": 0, "received": 0}

    headers = {"Content-Type": "application/json"}
    if API_KEY:
        headers["Authorization"] = f"Bearer {API_KEY}"

    try:
        resp = requests.post(
            f"{CHECKER_URL}/sites/add",
            json={"urls": urls},
            headers=headers,
            timeout=30,
            verify=False,
        )
        resp.raise_for_status()
        return resp.json()
    except Exception as e:
        print(f"❌ Failed to submit sites: {e}")
        return {"added": 0, "received": len(urls), "error": str(e)}


# ── Tempest scraper ─────────────────────────────────────────────────
def scrape_tempest(num_searches: int = 500) -> set[str]:
    """Run Tempest searches and return found myshopify URLs."""
    found = set()
    consecutive_failures = 0

    session = requests.Session()
    session.verify = False

    for i in range(num_searches):
        if consecutive_failures >= 30:
            print(f"⚠️ Too many failures ({consecutive_failures}), pausing...")
            time.sleep(5)
            consecutive_failures = 0

        query = random.choice(DORKS)
        offset = random.choice([0, 50, 100])

        params = {
            "q": query,
            "count": "50",
            "offset": str(offset),
        }
        if TEMPEST_COUNTRY:
            params["cc"] = TEMPEST_COUNTRY
            params["mkt"] = f"en-{TEMPEST_COUNTRY}"

        headers = {
            "User-Agent": IPAD_UA,
            "Accept": "application/json",
            "Accept-Language": "en-US,en;q=0.5",
            "Accept-Encoding": "gzip, deflate, br",
        }

        try:
            r = session.get(
                TEMPEST_V1_SEARCH,
                params=params,
                headers=headers,
                timeout=30,
                allow_redirects=True,
            )

            if r.status_code != 200 or not r.content:
                consecutive_failures += 1
                time.sleep(0.2)
                continue

            data = r.json()
            web_pages = data.get("webPages", {})
            results = web_pages.get("value", [])

            page_urls = set()
            for result in results:
                url = result.get("url", "")
                snippet = result.get("snippet", "") + " " + result.get("name", "")
                for text in [url, snippet]:
                    page_urls.update(extract_shopify_urls(text))

            # Also scan raw response text
            page_urls.update(extract_shopify_urls(r.text))

            if page_urls:
                consecutive_failures = 0
                new_urls = page_urls - found
                if new_urls:
                    found.update(new_urls)
                    print(f"🌐 [{i+1}/{num_searches}] +{len(new_urls)} sites (total: {len(found)})")
            else:
                consecutive_failures += 1

            time.sleep(random.uniform(0.1, 0.4))

        except Exception as e:
            consecutive_failures += 1
            time.sleep(0.2)
            continue

    return found


# ── Main loop ───────────────────────────────────────────────────────
def main():
    print(f"🚀 Scraper Worker starting")
    print(f"   Checker URL: {CHECKER_URL}")
    print(f"   Country: {TEMPEST_COUNTRY or 'Worldwide'}")
    print(f"   Searches per cycle: {SEARCHES_PER_CYCLE}")
    print(f"   Batch size: {BATCH_SIZE}")
    print(f"   Cycle delay: {CYCLE_DELAY}s")

    # Verify connectivity
    try:
        resp = requests.get(f"{CHECKER_URL}/health", timeout=10, verify=False)
        if resp.status_code == 200:
            print(f"✅ Checker API reachable")
        else:
            print(f"⚠️ Checker API returned {resp.status_code}")
    except Exception as e:
        print(f"⚠️ Cannot reach checker API: {e}")
        print(f"   Will keep retrying...")

    cycle = 0
    total_found = 0
    total_added = 0

    while True:
        cycle += 1
        print(f"\n{'='*50}")
        print(f"📡 Cycle {cycle} — Starting Tempest scrape...")
        print(f"{'='*50}")

        found = scrape_tempest(SEARCHES_PER_CYCLE)
        total_found += len(found)

        if found:
            # Submit in batches
            urls = list(found)
            for i in range(0, len(urls), BATCH_SIZE):
                batch = urls[i:i + BATCH_SIZE]
                result = submit_sites(batch)
                added = result.get("added", 0)
                total_added += added
                print(f"📤 Submitted {len(batch)} sites → {added} new")

        print(f"\n📊 Cycle {cycle} done: found {len(found)} sites this cycle")
        print(f"   Total found: {total_found} | Total new in DB: {total_added}")
        print(f"   Sleeping {CYCLE_DELAY}s before next cycle...")
        time.sleep(CYCLE_DELAY)


if __name__ == "__main__":
    main()
