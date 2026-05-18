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


# Bug fix for Open WebUI's SafeWebBaseLoader._fetch (as of the
# ghcr.io/open-webui/open-webui:main image, May 2026).
#
# The shipped _fetch passes `allow_redirects` BOTH inside
# `requests_kwargs` (set in __init__ as an SSRF mitigation) AND as an
# explicit kwarg to `session.get(...)`. aiohttp raises
# `TypeError: got multiple values for keyword argument 'allow_redirects'`
# on every URL. The outer try/except swallows that as the generic
# "Error fetching <url>, skipping due to continue_on_failure=True"
# warning, hiding the real cause. Result: every web-search URL fetch
# fails before any HTTP round-trip, the per-query Chroma collection
# is never created, and the model answers with no web context — so it
# hallucinates citations + content that look real.
#
# Sequencing matters: sitecustomize.py runs during `import site`, long
# before /app/backend is on sys.path, so we can't import
# `open_webui.retrieval.web.utils` directly here. Instead we wrap
# builtins.__import__ and apply the patch the first time the target
# module appears in sys.modules.
import sys as _sys
import builtins as _builtins


def _apply_safe_web_loader_patch():
    utils = _sys.modules.get("open_webui.retrieval.web.utils")
    if utils is None:
        return False
    SafeWebBaseLoader = getattr(utils, "SafeWebBaseLoader", None)
    if SafeWebBaseLoader is None:
        return False
    if getattr(SafeWebBaseLoader._fetch, "_vibe_patched", False):
        return True

    import aiohttp as _aiohttp
    import asyncio as _asyncio

    allow_redirects = getattr(utils, "AIOHTTP_CLIENT_ALLOW_REDIRECTS", True)
    session_ssl = getattr(utils, "AIOHTTP_CLIENT_SESSION_SSL", True)
    log = getattr(utils, "log", None)

    async def _patched_fetch(self, url, retries=3, cooldown=2, backoff=1.5):
        async with _aiohttp.ClientSession(trust_env=self.trust_env) as session:
            for i in range(retries):
                try:
                    # One merged kwargs dict — no duplicates passed to
                    # session.get(), which is what the upstream bug did.
                    kwargs = dict(self.requests_kwargs or {})
                    kwargs["headers"] = self.session.headers
                    kwargs["cookies"] = self.session.cookies.get_dict()
                    if not self.session.verify:
                        kwargs["ssl"] = False
                    else:
                        kwargs["ssl"] = session_ssl
                    kwargs["allow_redirects"] = allow_redirects

                    async with session.get(url, **kwargs) as response:
                        if self.raise_for_status:
                            response.raise_for_status()
                        return await response.text()
                except _aiohttp.ClientConnectionError as e:
                    if i == retries - 1:
                        raise
                    if log:
                        log.warning(
                            f"Error fetching {url} with attempt "
                            f"{i + 1}/{retries}: {e}. Retrying..."
                        )
                    await _asyncio.sleep(cooldown * backoff**i)
        raise ValueError("retry count exceeded")

    _patched_fetch._vibe_patched = True
    SafeWebBaseLoader._fetch = _patched_fetch
    return True


_original_import = _builtins.__import__
_patch_done = False
_in_patch = False


def _patched_import(name, globals=None, locals=None, fromlist=(), level=0):
    global _patch_done, _in_patch
    module = _original_import(name, globals, locals, fromlist, level)
    # Re-entrance guard: _apply_safe_web_loader_patch() does its own imports
    # (aiohttp, asyncio) which would recurse through this hook forever.
    if (
        not _patch_done
        and not _in_patch
        and "open_webui.retrieval.web.utils" in _sys.modules
    ):
        _in_patch = True
        try:
            if _apply_safe_web_loader_patch():
                _patch_done = True
                # Restore the original import so we stop paying the
                # per-import cost once the patch has landed.
                _builtins.__import__ = _original_import
        finally:
            _in_patch = False
    return module


_builtins.__import__ = _patched_import
