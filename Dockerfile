# Multi-stage build for both promscope binaries. CMD selects which:
#   docker build --build-arg CMD=promscope .
#   docker build --build-arg CMD=mock-vllm-exporter .
# Images are pinned by digest (supply-chain review finding): tags are
# mutable, digests are not. Bump deliberately, with the tag kept beside the
# digest as human-readable documentation.
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD=promscope
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# Distroless static: no shell, no package manager, ~2 MB of attack surface.
# Healthchecks exec the binary itself (promscope -healthcheck) because there
# is no curl to call.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/app /app
USER nonroot
ENTRYPOINT ["/app"]
