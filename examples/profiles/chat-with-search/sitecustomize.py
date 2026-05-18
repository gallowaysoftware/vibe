# Two fixes for Open WebUI's web-search pipeline, both applied via
# bind-mounted sitecustomize.py:
#
# 1) UA override. Open WebUI builds langchain's WebBaseLoader without
#    passing a header_template, so it inherits langchain's baked-in
#    browser User-Agent. Wikipedia (and most anti-scraping WAFs) 403
#    that exact pattern from an aiohttp client and return a 141-byte
#    "respect our robot policy" notice. We swap the UA for an
#    identifying string (configurable via WEB_LOADER_USER_AGENT).
#
# 2) SafeWebBaseLoader._fetch bug. The shipped _fetch passes
#    `allow_redirects` both inside `requests_kwargs` (set in __init__
#    as an SSRF mitigation) AND as an explicit kwarg to session.get.
#    aiohttp raises TypeError on every URL. The outer try/except logs
#    it as "Error fetching ... skipping due to continue_on_failure=True",
#    so it looks like a per-URL network error. Result: every page
#    fetch fails, Chroma collection is never populated, model gets
#    zero web context, and answers are hallucinations dressed up as
#    cited research.
#
# Why a sys.meta_path finder instead of wrapping builtins.__import__:
# the earlier version wrapped __import__ to lazily apply the loader
# patch when the target module showed up in sys.modules. That worked
# in isolation but reliably hung Open WebUI's first chat completion
# (the request was accepted, returned 200, then no LLM call ever
# fired). Suspected interaction between the synchronous patch path
# and FastAPI's async chat handler. A meta_path finder fires only
# when Python tries to resolve our exact target module, so the rest
# of the import system is untouched.
import os
import sys


_ua = os.environ.get("WEB_LOADER_USER_AGENT", "").strip()
if _ua:
    try:
        from langchain_community.document_loaders import web_base as _wb

        _wb.default_header_template["User-Agent"] = _ua
    except Exception:
        # Harmless in any env that doesn't ship langchain.
        pass


def _apply_safe_web_loader_patch(utils_module):
    SafeWebBaseLoader = getattr(utils_module, "SafeWebBaseLoader", None)
    if SafeWebBaseLoader is None:
        return
    if getattr(SafeWebBaseLoader._fetch, "_vibe_patched", False):
        return

    import aiohttp
    import asyncio

    allow_redirects = getattr(utils_module, "AIOHTTP_CLIENT_ALLOW_REDIRECTS", True)
    session_ssl = getattr(utils_module, "AIOHTTP_CLIENT_SESSION_SSL", True)
    log = getattr(utils_module, "log", None)

    async def _patched_fetch(self, url, retries=3, cooldown=2, backoff=1.5):
        async with aiohttp.ClientSession(trust_env=self.trust_env) as session:
            for i in range(retries):
                try:
                    # Single merged kwargs dict — explicit per-request
                    # fields override anything in requests_kwargs. The
                    # upstream bug passed allow_redirects both ways.
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
                except aiohttp.ClientConnectionError as e:
                    if i == retries - 1:
                        raise
                    if log:
                        log.warning(
                            f"Error fetching {url} with attempt "
                            f"{i + 1}/{retries}: {e}. Retrying..."
                        )
                    await asyncio.sleep(cooldown * backoff**i)
        raise ValueError("retry count exceeded")

    _patched_fetch._vibe_patched = True
    SafeWebBaseLoader._fetch = _patched_fetch


_TARGET_MODULE = "open_webui.retrieval.web.utils"


class _SafeWebLoaderPatchFinder:
    """sys.meta_path finder that wraps the target module's loader so
    we can apply the SafeWebBaseLoader._fetch patch immediately after
    Python finishes executing the module. Fires exactly once."""

    _done = False

    def find_spec(self, fullname, path, target=None):
        if self._done or fullname != _TARGET_MODULE:
            return None
        # Ask the other finders to resolve the spec. Skip ourselves so
        # this isn't infinite recursion.
        for finder in sys.meta_path:
            if finder is self:
                continue
            find = getattr(finder, "find_spec", None)
            if find is None:
                continue
            spec = find(fullname, path, target)
            if spec is None:
                continue
            # Wrap the loader's exec_module so our patch runs right
            # after Python finishes executing the module body.
            original_exec = spec.loader.exec_module

            def _exec(module, _orig=original_exec):
                _orig(module)
                try:
                    _apply_safe_web_loader_patch(module)
                except Exception:
                    pass
                self._done = True

            spec.loader.exec_module = _exec
            return spec
        return None


# Install if a target module hasn't already loaded. If it has — meaning
# this file is being re-imported in a process that already booted
# Open WebUI — patch directly.
if _TARGET_MODULE in sys.modules:
    try:
        _apply_safe_web_loader_patch(sys.modules[_TARGET_MODULE])
    except Exception:
        pass
else:
    sys.meta_path.insert(0, _SafeWebLoaderPatchFinder())
