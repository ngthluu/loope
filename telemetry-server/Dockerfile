# Build from the REPO ROOT (the build needs ../shared):
#   docker build -f telemetry-server/Dockerfile -t loope-telemetry-server .
# Run (all config is flags; -token is required):
#   docker run --rm -p 9090:9090 loope-telemetry-server -addr :9090 -token "$TOKEN"
#
# State is fully in-memory: no volume is needed. A restart shows an empty
# fleet until workers' next push repopulates it. TLS is not terminated here —
# put a reverse proxy in front for HTTPS.

FROM golang:1.25-alpine AS build
WORKDIR /src
# Only the two modules this binary compiles; the worker never enters the image.
COPY shared/ shared/
COPY telemetry-server/ telemetry-server/
ARG VERSION=dev
RUN cd telemetry-server && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/loope-telemetry-server .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/loope-telemetry-server /usr/local/bin/loope-telemetry-server
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/loope-telemetry-server"]
