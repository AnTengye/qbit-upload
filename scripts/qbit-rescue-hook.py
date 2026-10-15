#!/usr/bin/env python3
"""qBittorrent completion gate. Arguments: content path, name, infohash.

Install beside the original qbit-move.sh. Managed completions are published by
the host worker after verification. All other completions keep the old script.
The guard identity includes added_on, so removing/readding a hash is distinct.
"""
import http.cookiejar
import json
import os
from pathlib import Path
import re
import sys
import urllib.parse
import urllib.request


def main():
    if len(sys.argv) != 4 or not re.fullmatch(r"[0-9a-fA-F]{40}|[0-9a-fA-F]{64}", sys.argv[3]):
        raise ValueError("completion hook requires content path, name and hash")
    infohash = sys.argv[3].lower()
    guard_dir = Path(os.environ.get("QBIT_RESCUE_GUARDS", "/config/qbit-rescue/guards"))
    guard_file = guard_dir / (infohash + ".json")
    if guard_file.exists():
        # Fail closed for a managed hash if its record or API cannot be read.
        # It remains completed in qBittorrent for the host worker to recover.
        guard = json.loads(guard_file.read_text())
        if guard.get("hash") != infohash:
            raise ValueError("completion guard identity is invalid")
        base = os.environ.get("QBIT_RESCUE_LOCAL_API", "http://127.0.0.1:8080")
        opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
        )
        request = urllib.request.Request(
            base + "/api/v2/torrents/info?" + urllib.parse.urlencode({"hashes": infohash}),
            headers={"Referer": base + "/"},
        )
        with opener.open(request, timeout=10) as response:
            if response.status != 200 or "application/json" not in response.headers.get("Content-Type", ""):
                raise ValueError("completion API did not return JSON")
            torrents = json.loads(response.read(1_000_000))
        if len(torrents) != 1 or torrents[0].get("hash") != infohash:
            raise ValueError("completion torrent was not uniquely found")
        torrent = torrents[0]
        tags = {tag.strip() for tag in torrent.get("tags", "").split(",")}
        same_instance = guard.get("added_on", 0) > 0 and guard["added_on"] == torrent.get("added_on") and (guard.get("original") or guard.get("tag") in tags)
        pending_candidate = guard.get("added_on") == 0 and guard.get("tag") in tags
        if same_instance or pending_candidate:
            print("qbit-rescue: completed torrent retained for verified publication", infohash)
            return
    os.execv("/bin/bash", ["/bin/bash", "/config/qbit-move.sh", sys.argv[1], sys.argv[2]])


if __name__ == "__main__":
    main()
