# syntax=docker/dockerfile:1

# --- build ------------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache. There are three direct dependencies, so this is a small win — but it is
# also what keeps the layer stable enough to be worth caching at all.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# CGO_ENABLED=0 is what makes the distroless-static base below possible: the
# binary has no dynamic linker to find and no libc to match.
# -trimpath keeps build-machine paths out of panics.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/relay-eval ./cmd/relay-eval

# Staged here only so the runtime stage has something to COPY --chown. See the
# ledger note below: distroless has no shell, so mkdir in the final stage is not
# an option and this is the one way to give the path an owner.
RUN mkdir -p /out/data

# --- runtime ----------------------------------------------------------------
#
# distroless/static, not scratch. The adapters make TLS calls to
# api.openai.com and api.anthropic.com, and scratch ships no CA bundle — the
# failure is an x509 error at the first live request, long after the image
# looked fine. This base carries the certs and nothing else executable.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/relay /out/relay-eval /usr/local/bin/
COPY config/ /app/config/

# The ledger holds per-tenant cost data and is opened 0600 by uid 65532
# (distroless "nonroot"), so the directory it lives in has to be writable by
# that uid before the process starts.
#
# This COPY is doing real work and is not a no-op. A named volume is seeded from
# whatever the image has at the mount path, *including its ownership* — and if
# the path does not exist in the image, Docker creates it root-owned and the
# gateway dies at boot with "permission denied" before it ever listens. Declaring
# VOLUME alone does not create it. Verified by deleting these two lines: the
# container exits 1.
COPY --from=build --chown=nonroot:nonroot /out/data /app/data
VOLUME ["/app/data"]

EXPOSE 8080 9090
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/relay"]
