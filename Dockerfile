FROM golang:1.23-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /operator ./cmd/operator

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /operator /usr/local/bin/operator
USER 65534
ENTRYPOINT ["operator"]
