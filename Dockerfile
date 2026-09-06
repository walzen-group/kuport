# kuport-agent container image.
#
# Two stages: build with the official Go image, ship a static binary on
# scratch. The final image has no shell, no package manager, and nothing is
# fetched at container start. That is a deliberate repair. The predecessor
# this replaces ran on Alpine and installed iproute2 and nftables from an
# Alpine mirror on every pod start, so a node rebooting while the mirror was
# unreachable came up with no rules and the forward stayed down until the
# install worked. kuport-agent speaks netlink and nftables directly and needs
# no files around it to do that.
#
# No CA bundle is copied into the image. The agent talks to the in-cluster API
# server with the ServiceAccount token and the cluster CA, both mounted by the
# kubelet. If the agent ever needs outbound TLS to something else, this is the
# line to revisit.

# Pinned by tag and digest (Docker Hub, 2026-09-06). The build stage changes
# only when someone deliberately updates this pair.
FROM golang:1.26.8-alpine3.24@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS build

WORKDIR /src

# Resolve module downloads before copying sources so edits to Go code do not
# invalidate the dependency layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is stamped by CI (release.yaml passes the tag without the leading
# v). Local builds without a version stay honest about being unstamped.
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/kuport-agent ./cmd/kuport-agent

FROM scratch

# The agent needs NET_ADMIN to write nftables and netlink state on the host,
# which the DaemonSet grants. USER 0 states that intent in the image config;
# scratch has no other users to run as.
USER 0

COPY --from=build /out/kuport-agent /kuport-agent

ENTRYPOINT ["/kuport-agent"]
