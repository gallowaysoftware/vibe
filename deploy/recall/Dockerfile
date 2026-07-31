# recall: build from the vibe repo root (compose sets context: ../..).
# Runtime needs the git BINARY (the store shells out to git for every
# commit), so distroless is out; alpine + git is the floor.
FROM golang:1.26.5-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=$(cat VERSION 2>/dev/null || echo docker)" -o /recall ./cmd/recall

FROM alpine:3.20
RUN apk add --no-cache git ca-certificates
COPY --from=build /recall /recall
ENTRYPOINT ["/recall"]
