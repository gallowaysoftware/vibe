# toqui — AI travel companion (application)

toqui is topology.md's **UC3**: a self-hosted app whose LLM is the fleet.
Like riff it's a plain client of the hum front (`deploy/hum`) — it points at
hum's `:9000`, picks catalog model ids, and shares warm models with every
other client. It deploys and upgrades on its own lifecycle; all its data
(trips, bookings, chat) lives in its own postgres volume on `appdata`.

It also backs the **iOS app** (bring-your-own-server): the app asks for a
server URL on first launch — that's `TOQUI_API_URL` below.

## Files

- `docker-compose.yaml` — the stack: postgres + migrate + backend + frontend.
  Builds from a toqui checkout (`TOQUI_SRC`). House networking: backend + web
  each get a `br0` macvlan IP (the backend needs one to reach hum at `.211`,
  same macvlan-isolation reason as riff); postgres stays on an internal
  bridge. No host port maps.
- `.env.example` — copy to `.env`, fill the `REPLACE-` markers.

## Bring-up

1. **Check out toqui on the unraid box** (or `git pull` an existing copy) and
   point `TOQUI_SRC` at it:

   ```
   git clone https://github.com/gallowaysoftware/toqui \
     /mnt/user/appdata/toqui/src
   ```

2. **Create the `.env`** (copy `.env.example`), then:

   ```
   openssl rand -hex 32     # → JWT_SECRET
   openssl rand -hex 16     # → POSTGRES_PASSWORD (alphanumeric)
   ```

   Set `TOQUI_APP_URL` / `TOQUI_API_URL` to the two names you'll create in
   NPM, and `ALLOWED_EMAIL_DOMAINS` to your domain.

3. **Bring it up** (first run builds the images — a few minutes):

   ```
   docker compose up -d --build
   ```

   `migrate` runs once and exits 0; backend waits on it. Sanity from
   **another LAN box** (macvlan isolation — not the unraid shell):

   ```
   curl http://172.16.3.213:8090/healthz            # {"status":"ok"}
   curl -s http://172.16.3.213:8090/toqui.v1.AuthService/GetAuthProviders \
     -H 'content-type: application/json' -d '{}'     # {"emailPassword":true,...}
   ```

4. **NPM proxy hosts** (TLS via your existing certs). No Authelia in front —
   toqui has its own email+password auth, and the iOS app sends Bearer tokens
   the API must see directly:

   | Domain | → | Notes |
   |---|---|---|
   | `TOQUI_APP_URL` host | `http://172.16.3.214:8080` | the web app |
   | `TOQUI_API_URL` host | `http://172.16.3.213:8090` | the API (ConnectRPC; iOS + web both call it) |

   On the API host, allow WebSocket/streaming (chat is server-streamed) and
   **deny `/debug/` + gRPC reflection** at the proxy (see the security note).

5. **Register the first account** at `TOQUI_APP_URL` (email + a ≥12-char
   password). Open a trip and chat — the first message JIT-loads the model on
   the owning cell (streamed warming state); later messages share whatever the
   fleet already has warm.

6. **iOS app**: on first launch enter `TOQUI_API_URL`; it verifies `/healthz`
   and you're in. (TestFlight build is a separate task.)

## Model / tier mapping

toqui runs up to 7 sequential LLM calls per turn and can alternate FAST/SMART/
BEST call-to-call. **Rule (topology.md UC3): within one cell, all tiers map to
the same model or a co-resident group**, or every turn swap-storms the cell.
So `TOQUI_MODEL` sets all three tiers to one def, default `qwen3.6-27b-tools`.

- **Tools:** toqui is a heavy tool-loop client (create_itinerary_items,
  web_search, …). On Qwen it needs the `-tools` def (visible-content tool
  calls) — `qwen3.6-27b-tools`, not the base alias. Same rule as the
  distillery stacks.
- **Vision (caveat):** if trips use photos, image input arrives as `image_url`
  data-URLs and the tier model must be multimodal (`gemma-4-31b-mm` locally).
  There's no single co-resident tools+vision def in today's catalog, so it's
  one or the other until a spark model (dsv4flash has both) is up. Default to
  tools; switch `TOQUI_MODEL` to a vision def only if photo itineraries matter
  more than reliable tool calls.
- **Web search:** toqui degrades gracefully with no search backend. To wire
  it, run toqui's own SearXNG (its compose ships a `search` profile) or point
  `SEARXNG_URL` at a shared instance with JSON output enabled — not in this
  minimal stack; add it later if wanted.

## Security note (why TARGET_ENV stays `local`)

The stack doesn't set `TARGET_ENV`, so it defaults to `local`. That's
deliberate: toqui's web + iOS clients use **Bearer/localStorage tokens, not
cookies**, so the (non-local-only) hardcoded `.toqui.travel` cookie domain
never applies — forcing a non-local env would mis-set it for no benefit.

The one cost: `local` leaves `/debug/pprof` and gRPC reflection **enabled**.
Since the API is internet-exposed via NPM, **deny `/debug/` and reflection at
the proxy** (or keep the whole thing VPN-gated). A follow-up to make the
cookie domain env-driven would let this run a hardened non-local env; until
then, the proxy denies are the mitigation.

## Notes

- `POSTGRES_PASSWORD` must be alphanumeric — it's spliced into `DATABASE_URL`.
- All data is in the postgres bind mount (`appdata/toqui/postgres`). Back that
  up (`pg_dump`) and you've backed up everything.
- toqui's Claude/Gemini paths (if you ever set those keys) go direct to the
  provider, not through hum — only its OpenAI-compatible path is fleet-routed.
- Updating: `git pull` in `TOQUI_SRC`, then `docker compose up -d --build`.
