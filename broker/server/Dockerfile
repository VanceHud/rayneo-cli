# Multi-stage build for the zero-dependency RayNeo Token Broker server.
# Build from the repository root:
#   docker build -f broker/server/Dockerfile -t rayneo-token-broker .
#
# Run:
#   docker run -d --name rayneo-token-broker \
#     -p 127.0.0.1:8787:8787 \
#     -e BROKER_SECRET="your-strong-random-secret" \
#     -v rayneo-broker-data:/data \
#     rayneo-token-broker

FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod ./
COPY broker/server/ ./broker/server/

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/rayneo-broker \
    ./broker/server \
 && mkdir -p /data \
 && chmod 0700 /data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/rayneo-broker /rayneo-broker
COPY --from=builder --chown=65532:65532 /data /data

ENV BROKER_LISTEN=":8787"
ENV BROKER_STATE_FILE="/data/state.json"

EXPOSE 8787
VOLUME ["/data"]

USER 65532:65532
ENTRYPOINT ["/rayneo-broker"]
