# fleetd image: the vibe daemon binary on a minimal base. fleetd is a
# deployment, not a binary — no special build exists (fleet-control C1).
#
# Build the binary first (CGO-free, same as the release artifacts):
#   CGO_ENABLED=0 go build -trimpath -o deploy/fleetd/vibe ./cmd/vibe
# then:
#   docker build -t vibe-fleetd deploy/fleetd
# (deploy/fleetd/vibe is gitignored — never commit binaries.)
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY vibe /usr/local/bin/vibe
ENTRYPOINT ["vibe"]
