# Multi-stage build for both promscope binaries. CMD selects which:
#   docker build --build-arg CMD=promscope .
#   docker build --build-arg CMD=mock-vllm-exporter .
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD=promscope
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# Distroless static: no shell, no package manager, ~2 MB of attack surface.
# Healthchecks exec the binary itself (promscope -healthcheck) because there
# is no curl to call.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot
ENTRYPOINT ["/app"]
