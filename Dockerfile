# Build stage.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so that source edits do not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG VERSION=dev

# CGO is disabled so the result is a static binary that runs on a scratch base.
RUN CGO_ENABLED=0 GOFLAGS=-trimpath \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/kdn ./cmd/kdn

# Runtime stage.
#
# scratch, with only the binary and a CA bundle. The proxy's whole purpose is to
# stand in front of the API server and absorb hostile traffic, so there is no
# shell, package manager or interpreter in the image to be useful to anyone who
# finds a way in.
FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/kdn /kdn

# A non-root numeric UID; there is no /etc/passwd to resolve a name against.
USER 65532:65532

EXPOSE 8080 9090

ENTRYPOINT ["/kdn"]
CMD ["-config", "/etc/kdn/config.yaml"]
