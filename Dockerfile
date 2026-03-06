# ---- Build stage ----
FROM golang:1.25-alpine AS builder

# Install ca-certificates so HTTPS requests to S3 work securely
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .

# Build a statically linked binary (CGO_ENABLED=0)
# -s -w strips debugging info to make the binary even smaller
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o s3-exporter main.go

# Create a non-root user to run our app later
RUN adduser -D -g '' -u 10001 s3exporter

# ---- Runtime stage ----
FROM scratch

# Import certs, timezone data, and the non-root user from the builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group

# Copy the static binary
COPY --from=builder /src/s3-exporter /s3-exporter

# Run as the unprivileged user
USER s3exporter:s3exporter

# Default port for Prometheus to scrape
EXPOSE 9300

# Execute the binary
ENTRYPOINT ["/s3-exporter"]
