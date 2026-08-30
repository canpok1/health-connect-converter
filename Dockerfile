# Build stage
FROM golang:1 AS builder
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o health-connect-converter ./cmd/health-connect-converter

# Run stage
FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=builder /app/health-connect-converter .
COPY config.yaml .
# UID/GIDはplant-diaryのappuserに合わせて固定（mini-pcの実行ユーザーと一致させ、
# bind mountしたdataディレクトリの書き込み権限を揃えるため。経緯はADR 0006）
USER 1000:1000
ENTRYPOINT ["/app/health-connect-converter"]
