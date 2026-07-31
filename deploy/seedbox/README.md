# seedbox — qBittorrent for archive torrents

Runs as compose.manager project `seedbox` on hum. WebUI on the host
bridge at `:8085`; qBittorrent prints a fresh temp admin password to the
container log each start until one is set (`docker logs seedbox`).

What it serves (2026-07-31): the delisted Watchful1 per-subreddit Reddit
dump (Kyle's selective files ratio-capped at 2.0, plus ~505 GB of
contiguous-window altruism selection) and the Arctic Shift monthly
torrents (475 GB, uncapped — the maintainer asks for seeders). Speed
caps 30 MiB/s down / 12 MiB/s up. Total budget 1 TB on /archive.

Selection lesson: when selectively downloading scattered files from a
79,955-file torrent, piece granularity (64 MiB) drags in neighbor-file
data — random scatter cost ~250 GB of spillover. Select CONTIGUOUS
file-index runs; spillover then only occurs at run edges.
