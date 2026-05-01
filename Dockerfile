FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o go-sub-aggregator .

FROM alpine:3.21
RUN adduser -D -H subagg
WORKDIR /app
COPY --from=builder /build/go-sub-aggregator .
USER subagg
ENV CONFIG_FILE=/config/config.yaml
EXPOSE 8000
ENTRYPOINT ["./go-sub-aggregator"]
