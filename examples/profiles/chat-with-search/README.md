# Chat profile: local Gemma 3 (vision) + Open WebUI + SearXNG + Tier-1 RAG

A vibe profile that brings up a Claude/ChatGPT-style chat experience
against a local **multimodal** model: Gemma 3 27B-it at Q6_K (text +
image input) served by llama-server, Open WebUI for the UI, SearXNG
sidecar for "turbo Google" web search, and Open WebUI's built-in RAG
(BGE-M3 embeddings + BGE-reranker-v2-m3 reranker + hybrid BM25+vector
search) for chatting with your own documents. Profile targets a 32 GB
GPU; drop to Q4_K_M for 24 GB or to 12B for 16 GB (see Customizing).

The whole stack is two containers managed by `vibe start chat`. State
(SQLite chat history, ChromaDB vectors, uploads, embedding model
cache) lives at `$XDG_STATE_HOME/vibe/frontend/chat/data` as bind-
mounted files you can back up with `tar` or inspect with `sqlite3`.

## Install

```sh
# 1. Drop the profile.
mkdir -p ~/.config/vibe/profiles ~/.config/vibe/compose/chat
cp chat.yaml ~/.config/vibe/profiles/chat.yaml

# 2. Drop the compose file + SearXNG config alongside.
cp docker-compose.yaml ~/.config/vibe/compose/chat/docker-compose.yaml
cp -r searxng ~/.config/vibe/compose/chat/

# 3. Generate a real SearXNG secret_key. The placeholder is rejected
#    at startup so you can't accidentally skip this.
sed -i "s/REPLACE-WITH-RAND-HEX-32/$(openssl rand -hex 32)/" \
  ~/.config/vibe/compose/chat/searxng/settings.yml

# 4. Edit ~/.config/vibe/profiles/chat.yaml: point backend.llama_server
#    at the GGUF model you want, fix the absolute paths under frontend.
#    The REPLACE: markers call out the lines you need to change.

# 5. Bring it up. First boot pulls 2 images (~1.6 GB), downloads
#    Playwright's Chromium (~500 MB, for web-search scraping), the
#    embedding model (~2.3 GB on first knowledge upload), and may
#    pull the LLM weights from HuggingFace. Subsequent starts are
#    fast — everything is cached.
vibe start chat
# → browser to http://127.0.0.1:8080
```

## What's in here

| File | Goes to | What it is |
|---|---|---|
| `chat.yaml` | `~/.config/vibe/profiles/chat.yaml` | The vibe profile (model + frontend block). |
| `docker-compose.yaml` | `~/.config/vibe/compose/chat/docker-compose.yaml` | Open WebUI + SearXNG. |
| `searxng/settings.yml` | `~/.config/vibe/compose/chat/searxng/settings.yml` | SearXNG config (replace secret_key). |
| `smoke.yaml` | (run in place) | vamp smoke pipeline: probes SearXNG, Wikipedia, Open WebUI, the vibe proxy, and a 1-token LLM round-trip. |

## Using it

- **Plain chat** — pick the model in the top-left dropdown and start
  typing. The LLM streams via the OpenAI-compatible proxy at :9000.
- **Image input** — attach an image to a chat message (paperclip /
  drag-and-drop). Open WebUI ships the image to the OpenAI-compat
  `/v1/chat/completions` endpoint as a `image_url` content part;
  llama-server uses the mmproj projector to encode it before passing
  the joint token sequence to Gemma 3. Works in any chat.
- **Web search** — toggle the globe icon in the chat composer
  (Workspace → Settings → Web Search → enabled). The `#web` prefix
  is *not* a real Open WebUI trigger — the globe toggle is the only
  reliable way to dispatch the query to SearXNG and have the top
  results included as retrieval context.
- **RAG over your docs** — Workspace → Knowledge → New, upload PDFs /
  Markdown / text. Attach the knowledge collection to a model
  (Workspace → Models → Edit) or reference it per-chat with `#`.
- **Multiple knowledge collections** — recommended over one big
  bucket. Retrieval quality degrades as the corpus grows mixed;
  separating `work`, `code`, `personal` collections and attaching the
  right ones per-question is the higher-signal pattern.

## State

```
~/.local/state/vibe/frontend/chat/data/
├── webui.db, .db-shm, .db-wal     SQLite — chats, settings, knowledge meta
├── vector_db/                     ChromaDB — RAG embeddings
├── uploads/                       raw attached files
└── cache/                         downloaded embedding model weights
```

Inspect: `sqlite3 ~/.local/state/vibe/frontend/chat/data/webui.db`.
Back up: `tar czf chat-state.tar.gz -C ~/.local/state/vibe/frontend chat`.

## Smoke test

Once the profile is running (`vibe start chat`), check every layer of
the stack in one command:

```sh
vamp run examples/profiles/chat-with-search/smoke.yaml
```

Hits SearXNG, scrapes Wikipedia with an identifying UA (catches
bot-policy regressions), confirms Open WebUI is serving its shell,
asks the vibe proxy for `/v1/models`, and fires a 1-token LLM
round-trip ("reply PONG"). Total wall time ~1–2s. Every stage is a
`webhook` with an `assert:` block — exit code 0 = the stack is
healthy end-to-end. See the file header for what's deliberately not
covered (the OWUI search API needs a JWT we can't bootstrap from a
fresh deploy without a UI round-trip).

## Why these choices

**SearXNG over DuckDuckGo / Tavily / Brave** — self-hosted, no API
key, no per-query cost, aggregates multiple upstream engines, JSON
output mode is purpose-built for LLM consumption.

**Playwright over Open WebUI's default `safe_web` scraper** — Open
WebUI's default uses langchain's `WebBaseLoader` (aiohttp) with a
custom override whose `_fetch` has a latent `allow_redirects`
TypeError, no per-request timeout (single slow URL hangs the whole
search), and a User-Agent that Wikipedia's bot policy rejects.
Playwright drives real Chromium — no UA dance, no JS-rendering
gaps, supported scraping path. Costs ~500 MB Chromium download on
first start and slower per-query render; worth it.

**BGE-M3 over OpenAI / smaller models** — multilingual, hybrid
dense+sparse+ColBERT representations, currently best-in-class for
self-hosted RAG, runs on CPU at ~30 docs/sec so it doesn't compete
with the LLM for GPU. Cached on first use.

**Tier 1 (in-process Chroma) over Tier 2 (Qdrant + TEI)** — fewer
moving parts; good through ~10K chunks. When you outgrow it, swap
to a dedicated `rag` profile with TEI for embeddings and Qdrant for
storage; the chat profile keeps its in-process setup for casual use.

## Customizing

- **Different LLM**: change `backend.llama_server.{path, huggingface,
  alias, context}` in `chat.yaml`. The frontend doesn't care which
  model serves the OpenAI-compat endpoint.
- **Text-only model**: drop the `mmproj` field and the
  `huggingface.mmproj_file` entry. llama-server will start without
  multimodal support and Open WebUI will just hide the image
  attach affordance for that model.
- **Swap vision model**: any GGUF model with a matching mmproj works
  (Qwen2.5-VL, LLaVA, InternVL3, MiniCPM-V). Point `path` at the
  weights, `mmproj` at the projector, and update both `huggingface`
  files. Setting `mmproj_file` without `mmproj` (or pointing `mmproj`
  at a non-existent path with no HF entry) is rejected at load.
- **Different chunk size / top-k**: edit the `CHUNK_SIZE`,
  `CHUNK_OVERLAP`, `RAG_TOP_K` env vars in the compose file. Code
  collections want smaller chunks (200/50); long-form prose wants
  larger (1500/300).
- **Disable web search**: drop the `searxng:` service from the compose
  file and set `ENABLE_RAG_WEB_SEARCH=False`. Two-line change.
- **Reset chat history**: stop the profile, `rm -rf
  ~/.local/state/vibe/frontend/chat/data/`, start again. The state
  dir gets recreated on activate.
