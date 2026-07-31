"""
Recall digest inlet filter — the INJECTION half of the memory system.

Fetches http://recall/digest?expert=<slug> at chat start and prepends it
as system context, so the expert model begins every conversation already
knowing the user's profile, goals, recent decisions, and where the last
session left off. The MCP tools are the other half (deep recall +
writes); injection is what makes recall reliable — a model cannot
search for what it doesn't know it forgot.

Install: Admin Panel -> Functions -> "+" -> paste this file -> save,
then enable it on each expert workspace model (model editor -> Filters).
Model-to-expert mapping lives in the MODEL_EXPERTS valve; models not in
the mapping pass through untouched, and any recall outage fails open —
the chat proceeds without memory rather than erroring.

Python is mandatory here (Open WebUI functions are Python-only); this
is deliberately the thinnest possible shim over the Go service.
"""

import json
import urllib.request

from pydantic import BaseModel, Field


class Filter:
    class Valves(BaseModel):
        RECALL_URL: str = Field(
            default="http://172.16.3.215:8092",
            description="Base URL of the recall service",
        )
        MODEL_EXPERTS: str = Field(
            default=json.dumps(
                {
                    "x4-expert": "x4",
                    "elite-expert": "elite",
                    "locallm-expert": "locallm",
                    "distillery-expert": "distillery",
                }
            ),
            description="JSON map of workspace-model id -> recall expert slug",
        )
        TIMEOUT_SECONDS: float = Field(
            default=3.0,
            description="Digest fetch timeout; on expiry the chat proceeds without memory",
        )

    def __init__(self):
        self.valves = self.Valves()

    def _expert_for(self, model_id: str) -> str | None:
        try:
            return json.loads(self.valves.MODEL_EXPERTS).get(model_id)
        except (ValueError, AttributeError):
            return None

    def _fetch_digest(self, expert: str) -> str | None:
        url = f"{self.valves.RECALL_URL.rstrip('/')}/digest?expert={expert}"
        try:
            with urllib.request.urlopen(url, timeout=self.valves.TIMEOUT_SECONDS) as resp:
                if resp.status != 200:
                    return None
                return resp.read().decode("utf-8", errors="replace")
        except Exception:
            return None  # fail open: no memory beats no chat

    def inlet(self, body: dict, __user__: dict | None = None) -> dict:
        expert = self._expert_for(body.get("model", ""))
        if not expert:
            return body
        digest = self._fetch_digest(expert)
        if not digest:
            return body

        block = (
            "The following is your persistent memory for this user in this "
            "domain, maintained via the recall tools:\n\n" + digest.strip()
        )
        messages = body.get("messages", [])
        # Fold into an existing leading system message rather than adding a
        # second one — multi-system handling varies by chat template.
        if messages and messages[0].get("role") == "system":
            messages[0]["content"] = f"{messages[0]['content']}\n\n{block}"
        else:
            messages.insert(0, {"role": "system", "content": block})
        body["messages"] = messages
        return body
