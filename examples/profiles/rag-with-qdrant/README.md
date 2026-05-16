# Rag profile: local LLM + Open WebUI + TEI + Qdrant

A vibe profile that brings up a Tier-2 RAG stack against a local model:
Open WebUI for the UI, [TEI](https://github.com/huggingface/text-embeddings-inference)
for dedicated embedding (BGE-M3 over an OpenAI-compatible API), and
[Qdrant](https://qdrant.tech/) for vector storage. Reranking
(BGE-reranker-v2-m3) and hybrid BM25+vector search are on.

Three containers managed by `vibe start rag`. State (Open WebUI's
SQLite + uploads, TEI's embedding model cache, Qdrant's vector
storage) lives at `$XDG_STATE_HOME/vibe/frontend/rag/` as bind-mounted
files.

When to pick `rag` over `chat-with-search`:
- Indexing thousands of documents — TEI is faster and more parallel
  than in-process SentenceTransformers.
- You want vector observability — `curl http://127.0.0.1:6333/...`
  to inspect collections, dump backups via Qdrant snapshots.
- You want to share the same vector store across multiple frontends.

Otherwise `chat-with-search` (Tier 1, in-process Chroma) is simpler
and just as good for casual use.

## Install

```sh
# 1. Drop the profile.
mkdir -p ~/.config/vibe/profiles ~/.config/vibe/compose/rag
cp rag.yaml ~/.config/vibe/profiles/rag.yaml

# 2. Drop the compose file.
cp docker-compose.yaml ~/.config/vibe/compose/rag/docker-compose.yaml

# 3. Edit ~/.config/vibe/profiles/rag.yaml: point backend.llama_server
#    at the GGUF model you want, fix the compose_file path. The
#    REPLACE: markers call out the lines you need to change.

# 4. Bring it up. First boot pulls 3 images (~3 GB), downloads
#    BGE-M3 weights (~2.3 GB) on TEI's first run, and may pull the
#    LLM weights from HuggingFace.
vibe start rag
# → browser to http://127.0.0.1:8080
```

## What's in here

| File | Goes to | What it is |
|---|---|---|
| `rag.yaml` | `~/.config/vibe/profiles/rag.yaml` | The vibe profile (model + frontend block). |
| `docker-compose.yaml` | `~/.config/vibe/compose/rag/docker-compose.yaml` | Open WebUI + TEI + Qdrant. |

## Port layout

| Port | Service | Owner |
|---|---|---|
| 9000 | OpenAI-compat proxy (chat LLM) | **vibe** |
| 9001 | Control plane | **vibe** |
| 8080 | Open WebUI | this stack |
| 14002 | TEI embeddings (OpenAI-compat) | this stack |
| 14003 | TEI Prometheus metrics | this stack |
| 6333 | Qdrant REST | this stack |
| 6334 | Qdrant gRPC | this stack |

**Gotcha worth knowing:** TEI defaults its Prometheus metrics endpoint
to **port 9000**, which collides with vibe's OpenAI-compat proxy.
TEI's main HTTP port defaults to 8081 (Sonatype Nexus collision risk)
and TEI's Prometheus default 9100 collides with node_exporter. The
compose file overrides both via `--port=14002` and
`--prometheus-port=14003` to land somewhere low-collision. Without
those overrides TEI crash-loops on "Address already in use" and the
`wait_for` times out with an opaque "TEI didn't come up" error.

## State

```
~/.local/state/vibe/frontend/rag/
├── webui/             Open WebUI's SQLite + uploads + cache
├── tei-cache/         BGE-M3 weights (downloaded once, ~2.3 GB)
└── qdrant/            Qdrant vector storage (binary + WAL)
```

Inspect Qdrant directly:

```sh
curl http://127.0.0.1:6333/collections
curl http://127.0.0.1:6333/collections/<name>/points/scroll \
  -X POST -H 'Content-Type: application/json' -d '{"limit":3}'
```

Back up the lot:

```sh
tar czf rag-state.tar.gz -C ~/.local/state/vibe/frontend rag
```

## Customizing

- **TEI on GPU**: change the image tag to
  `ghcr.io/huggingface/text-embeddings-inference:cuda-1.9` and add a
  `deploy.resources.reservations` block for nvidia. BGE-M3 only needs
  ~1.2 GB VRAM and you'll get ~10x throughput.
- **Different embedding model**: swap `--model-id` in TEI's command
  + `RAG_EMBEDDING_MODEL` in Open WebUI's env. Both must agree on
  vector dimensionality (BGE-M3 = 1024).
- **Qdrant snapshots / replication**: see
  https://qdrant.tech/documentation/concepts/snapshots/ — vibe
  doesn't manage these for you. The data is just files in the
  bind-mounted volume.
