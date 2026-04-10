# syntax=docker/dockerfile:1
# Force Linux/amd64 so builds on Windows do not target the host OS/arch.
FROM --platform=linux/amd64 golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM --platform=linux/amd64 alpine:3.22.0

RUN apk add --no-cache ca-certificates tzdata

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone \
    && chmod +x /CLIProxyAPI/CLIProxyAPI \
    && _elf=$(od -An -tx1 -N4 /CLIProxyAPI/CLIProxyAPI | tr -d ' \n') \
    && test "$_elf" = "7f454c46" || (echo "CLIProxyAPI: not a Linux ELF binary (magic=$_elf)" >&2; exit 1)

CMD ["./CLIProxyAPI"]
