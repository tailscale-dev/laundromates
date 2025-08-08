FROM golang:latest AS builder

WORKDIR /app

COPY . .

RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -a -o laundromates .

FROM alpine:latest

RUN apk --no-cache add ca-certificates

COPY --from=builder /app/laundromates /usr/local/bin/laundromates

WORKDIR /app

ENTRYPOINT ["/usr/local/bin/laundromates"]