#!/usr/bin/env bash
# C7a partials, part 2 — the same claims read through the ACTUAL document
# shapes (/api/fleet/usage returns `buckets`; an activity row nests `tokens`).
set -uo pipefail
source "$(dirname "$0")/gl.sh"

FRONT_PORT=$(cell_port front); ALPHA_PORT=$(cell_port alpha)
LEDGER=$LAB/state/fleetd/vibe/fleet/usage.jsonl
FRONT_STATE=$LAB/state/ann-front

usage() { curl -fsS -m 20 -H "Authorization: Bearer $VIBE_TOKEN" "$VIBE_API/api/fleet/usage"; }
alpha_chat() { usage | jq -c '[.buckets[]|select(.cell=="alpha" and .model=="lab-chat" and .basis=="chat")]|{req:(map(.req)|add),poke:(map(.poke_req)|add),out:(map(.out)|add),in_fresh:(map(.in_fresh)|add)}'; }

hr "1. gate 2 (live half): the front COLLECTED usage and the fold refused it by name"
echo "# what the front cell's own collector reported (cumulative, on the wire):"
jq -c '{last_row_id, models:[.models[]|{model,basis,req,in_fresh,in_cached,out,poke_req,err_req}]}' "$FRONT_STATE/vibe/fleet/cell-usage.json"
echo "# what the front's llama-swap logged:  total=$(curl -fsS -m 10 "http://127.0.0.1:$FRONT_PORT/api/metrics/activity?limit=1" | jq -r .total)"
echo "# buckets in /api/fleet/usage attributed to the front: $(usage | jq -c '[.buckets[]|select(.cell=="front")]|length')"
echo "# every cell that DOES have buckets: $(usage | jq -c '[.buckets[].cell]|unique')"
echo "# usage.jsonl lines carrying cell=front: $(grep -c '"cell":"front"' "$LEDGER" || true)  (of $(wc -l <"$LEDGER") lines)"
echo "# alpha's row, which is where those 10 front-hop requests were served:"
alpha_chat

hr "2. gate 5: a poke that is NOT one token"
B=$(alpha_chat); echo "# before: $B"
curl -fsS -m 300 "http://127.0.0.1:$ALPHA_PORT/v1/chat/completions" -H 'Content-Type: application/json' \
  -d '{"model":"lab-chat","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' | jq -c '{completion:.usage.completion_tokens}'
sleep 30
M=$(alpha_chat); echo "# after the 1-token completion: $M"
curl -fsS -m 300 "http://127.0.0.1:$ALPHA_PORT/v1/chat/completions" -H 'Content-Type: application/json' \
  -d '{"model":"lab-chat","messages":[{"role":"user","content":"count to four slowly"}],"max_tokens":6}' | jq -c '{completion:.usage.completion_tokens}'
sleep 30
A=$(alpha_chat); echo "# after the multi-token completion: $A"
echo "# the two llama-swap rows behind that:"
curl -fsS -m 10 "http://127.0.0.1:$ALPHA_PORT/api/metrics/activity" | jq -c '[.data[]|select(.model=="lab-chat")]|sort_by(.id)|.[-2:][]|{id,out:.tokens.output_tokens,in:.tokens.input_tokens,cache:.tokens.cache_tokens,status:.resp_status_code}'

hr "3. gate 5: C8 probe traffic is a second self-traffic producer, metered as ordinary traffic"
echo "# alpha, chat probes (64 output tokens each):"
curl -fsS -m 10 "http://127.0.0.1:$ALPHA_PORT/api/metrics/activity" | jq -c '[.data[]|select(.model=="lab-chat" and .tokens.output_tokens==64)]|{n:length,tokens:(map(.tokens.input_tokens+.tokens.output_tokens)|add)}'
curl -fsS -m 10 "http://127.0.0.1:$ALPHA_PORT/api/metrics/activity" | jq -r '[.data[]|select(.model=="lab-chat" and .tokens.output_tokens==64)] | if length>0 then ((map(.tokens.input_tokens+.tokens.output_tokens)|add)/length) as $p | "  \($p) tokens per chat probe x 96/day cap = \(($p*96)|floor) tokens/cell/day" else "  (none)" end'
echo "# charlie, embed probes (64-input batches):"
curl -fsS -m 10 "http://127.0.0.1:$(cell_port charlie)/api/metrics/activity" | jq -c '[.data[]|select(.req_path=="/v1/embeddings" and .tokens.input_tokens>=300)]|{n:length,tokens:(map(.tokens.input_tokens)|add)}'
echo "# and how the ledger classified them (none of these is a poke):"
usage | jq -c '[.buckets[]|select(.basis=="chat" or .basis=="embed")|{cell,model,basis,req,poke_req,err_req,unmeasured_req,out,in_fresh}]'
hr done
