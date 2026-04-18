FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
RUN go build -o /app/github2snipe .

FROM alpine:3.21
COPY --from=builder /app/github2snipe /app/github2snipe
ENTRYPOINT ["/app/github2snipe"]
