# Build stage
FROM golang:1.24 AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o hc-export ./cmd/hc-export

# Run stage
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=builder /app/hc-export .
USER nonroot:nonroot
ENTRYPOINT ["/app/hc-export"]
