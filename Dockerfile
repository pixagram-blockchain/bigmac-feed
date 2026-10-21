FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o bigmac-feed .

FROM alpine:3.24
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/bigmac-feed /usr/local/bin/bigmac-feed
ENTRYPOINT ["bigmac-feed"]
