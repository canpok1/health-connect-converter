# Build stage
FROM golang:1.24 AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o health-connect-converter ./cmd/health-connect-converter

# Run stage
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=builder /app/health-connect-converter .
USER nonroot:nonroot
ENTRYPOINT ["/app/health-connect-converter"]
