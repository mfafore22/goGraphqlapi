# syntax=docker/dockerfile:1
# Multi-stage build. The final image contains only the compiled binary and
# the static frontend — no toolchain, no source. Keeps the image small and
# reduces attack surface.

# ---- Stage 1: build ----
FROM golang:1.22-alpine AS build
WORKDIR /src

# Copy module files first and download deps. This layer is cached as long
# as go.mod/go.sum don't change, which makes rebuilds on code-only changes fast.
COPY go.mod go.sum* ./
RUN go mod download

# Now copy the rest of the source and compile.
COPY . .
# CGO_ENABLED=0 produces a fully static binary — no libc required at runtime.
# -trimpath strips local filesystem paths from the binary (reproducible builds,
#   and no accidental leakage of /home/claude paths in stack traces).
RUN CGO_ENABLED=0 go build -trimpath -o /out/taskflow ./cmd/server

# ---- Stage 2: runtime ----
FROM alpine:3.20
# ca-certificates lets the app make outbound HTTPS calls (not needed today,
# but it's a one-layer hedge against future needs).
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/taskflow /app/taskflow
COPY web /app/web
EXPOSE 8080
ENTRYPOINT ["/app/taskflow"]
