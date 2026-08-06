#!/usr/bin/env bash
# C10 13d — the full await primitive across two shells, with the lease handshake.
#   A waits idle+unleased, claims a lease; B (a genuinely separate process,
#   started while A's lease is live) must NOT unblock until A's lease clears.
set -uo pipefail
source "$(dirname "$0")/gl.sh"

CELL=bravo
leases() { curl -fsS -m 10 -H "Authorization: Bearer $VIBE_TOKEN" "$VIBE_API/api/fleet/leases" | jq -c '[.leases[]|{cell,model,holder,expires_at,hold}]'; }

hr "0. baseline: no leases, and a real request so the idle window is a measured one"
leases
curl -fsS -m 180 http://127.0.0.1:9642/v1/embeddings -H 'Content-Type: application/json' \
  -d '{"model":"lab-embed-b","input":"C17 13d activity edge"}' | jq -c '{usage}'
state | jq -c ".cells[]|select(.name==\"$CELL\")|.activity"

hr "1. shell A: await --model lab-embed-b --ready --idle 20s --unleased --lease batchA (ttl 90s)"
A0=$(date +%s)
$BIN cell await $CELL --model lab-embed-b --ready --idle 20s --unleased --lease batchA --lease-ttl 90s \
  --lease-note "C17 13d batch A" --timeout 3m; rcA=$?
A1=$(date +%s)
echo "# A rc=$rcA after $((A1-A0))s"
leases

hr "2. shell B starts NOW, in its own process, same flags, holder batchB"
B0=$(date +%s)
( $BIN cell await $CELL --model lab-embed-b --ready --idle 20s --unleased --lease batchB --lease-ttl 60s --timeout 5m \
    > "$LAB/logs/13d-B.out" 2>&1; echo $? > "$LAB/logs/13d-B.rc" ) &
BPID=$!
for i in 1 2 3; do
  sleep 20
  echo "# t+$((i*20))s  B alive=$(kill -0 $BPID 2>/dev/null && echo yes || echo no)  leases=$(leases)"
done

hr "3. A's own re-run ignores its OWN holder (must not deadlock on its residue)"
A2=$(date +%s)
timeout 40 $BIN cell await $CELL --model lab-embed-b --ready --idle 20s --unleased --lease batchA --lease-ttl 90s --timeout 30s
echo "# A-rerun rc=$? after $(( $(date +%s) - A2 ))s (own holder ignored => fast)"

hr "4. wait for B"
wait $BPID
B1=$(date +%s)
echo "# B rc=$(cat "$LAB/logs/13d-B.rc") after $((B1-B0))s elapsed"
cat "$LAB/logs/13d-B.out"
leases

hr "5. cleanup: drop both leases"
for h in batchA batchB; do
  curl -fsS -m 10 -H "Authorization: Bearer $VIBE_TOKEN" -X DELETE \
    "$VIBE_API/api/fleet/lease" -H 'Content-Type: application/json' \
    -d "{\"cell\":\"$CELL\",\"model\":\"lab-embed-b\",\"holder\":\"$h\"}"; echo
done
leases
hr done
