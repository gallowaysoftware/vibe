# Override langchain WebBaseLoader's default User-Agent so Open WebUI's
# web-search URL scraper doesn't get 403'd by anti-scraping policies.
#
# Why this exists: Open WebUI's SafeWebBaseLoader (retrieval/web/utils.py)
# is constructed without a header_template, so it inherits langchain's
# baked-in Chrome UA. Wikipedia (and many other sites) treat that exact
# string + an aiohttp client as a non-compliant scraper and return a
# 403 with a 141-byte "respect our robot policy" body. The empty fetch
# leaves the per-query Chroma collection empty, retrieval returns no
# context, and the LLM answers with zero web context — i.e. hallucinates
# from training data even though SearXNG and the loader both "succeeded".
#
# Per Wikipedia's robot policy and most WAFs, an identifying UA
# (project name + contact / repo URL) is the expected pattern and
# returns 200. Configurable via WEB_LOADER_USER_AGENT env var so this
# file stays generic.
#
# Site-wide patch: sitecustomize.py is auto-imported by Python during
# `site` initialization, before any application code runs. Mounted into
# the container at /usr/local/lib/python3.11/site-packages/sitecustomize.py.
import os


_ua = os.environ.get("WEB_LOADER_USER_AGENT", "").strip()
if _ua:
    try:
        from langchain_community.document_loaders import web_base as _wb

        _wb.default_header_template["User-Agent"] = _ua
    except Exception:
        # Don't break Python startup if langchain isn't installed — this
        # file is harmless in any environment that doesn't ship it.
        pass
