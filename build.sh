#!/bin/sh
export PATH="/root/go-sdk/go/bin:$PATH"
go build -o mautrix-datamachine ./cmd/mautrix-datamachine "$@"
