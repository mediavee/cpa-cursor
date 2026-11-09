# Image of cursor-relay: a static binary on a non-root base that carries the CA certificates.
# Build from the repository root: docker build -f cmd/cursor-relay/Dockerfile .
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/cursor-relay ./cmd/cursor-relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cursor-relay /cursor-relay
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/cursor-relay"]
