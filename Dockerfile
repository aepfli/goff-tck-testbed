# Same multi-stage shape as flagd-testbed: take the vendor's own binary unmodified and add a
# launchpad as PID 1. The launchpad supervises the relay proxy, which keeps POST /stop meaning
# "kill the backend process" rather than "stop the container" -- the control API requires that,
# because dynamically mapped host ports do not survive a container restart.
#
# The vendor image is distroless; its binary is static, so it runs unchanged on busybox, which
# buys a shell for `docker exec` debugging and costs nothing else.

ARG GOFF_VERSION=v1.57.0

FROM gofeatureflag/go-feature-flag:${GOFF_VERSION} AS goff

FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY launchpad/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /launchpad .

FROM busybox:1.36
COPY --from=goff /go-feature-flag /go-feature-flag
COPY --from=builder /launchpad /launchpad
COPY flags/ /flags/
COPY launchpad/configs/ /configs/
# The launchpad writes goff-proxy.yaml and the flag file into /goff at runtime.
RUN mkdir -p /goff && chown -R nobody /goff /flags /configs
USER nobody
# 1031: GOFF relay proxy (what the provider under test talks to: OFREP, /v1/flag/configuration, ...)
# 8080: launchpad control API
EXPOSE 1031 8080
ENTRYPOINT ["/launchpad"]
