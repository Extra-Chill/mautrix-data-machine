FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates build-base olm-dev

COPY . /build
WORKDIR /build
RUN go build -o mautrix-datamachine ./cmd/mautrix-datamachine

FROM alpine:3.23

ENV UID=1337 \
    GID=1337

RUN apk add --no-cache su-exec ca-certificates olm bash jq yq-go curl

COPY --from=builder /build/mautrix-datamachine /usr/bin/mautrix-datamachine
COPY docker-run.sh /docker-run.sh
RUN chmod +x /docker-run.sh

VOLUME /data

CMD ["/docker-run.sh"]
