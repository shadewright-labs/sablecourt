#!/usr/bin/env bash
# Tests of hack/check-dockerfiles.sh on fixture repositories, and on this repository. Usage:
#   hack/test/dockerfiles_test.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fails=0 n=0

case_() { # name want path content
  local repo="${work}/r$((n += 1))" got=0
  mkdir -p "$(dirname "${repo}/$3")"
  printf '%s\n' "$4" > "${repo}/$3"
  git -C "${repo}" init -q && git -C "${repo}" add -A
  "${root}/hack/check-dockerfiles.sh" "${repo}" > /dev/null 2>&1 || got=$?
  if [ "${got}" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1: exit ${got}, want $2"; fails=$((fails + 1)); fi
}

case_ "go build without GOWORK=off" 1 Dockerfile 'FROM golang AS build
COPY . .
RUN CGO_ENABLED=0 go build ./cmd/x'
case_ "go build after ENV GOWORK=off" 0 Dockerfile 'FROM golang AS build
ENV GOWORK=off
RUN go build ./cmd/x'
case_ "setting from an earlier stage does not carry over" 1 Dockerfile 'FROM golang AS deps
ENV GOWORK=off
RUN go mod download
FROM golang AS build
RUN go build ./cmd/x'
case_ "go mod download before the setting" 1 Dockerfile 'FROM golang AS build
RUN go mod download
ENV GOWORK=off'
case_ "go after a shell operator" 1 sub/Dockerfile 'FROM golang
RUN cd /src && go build .'
case_ "GOWORK=auto is not off" 1 Dockerfile 'FROM golang
ENV GOWORK=auto
RUN go build .'
case_ "a package named like go is not the go command" 0 Dockerfile 'FROM alpine
RUN apk add go-bindata'
case_ "no go at all" 0 Dockerfile 'FROM scratch
COPY x /x'
case_ "go on a continued RUN line" 1 Dockerfile 'FROM golang AS build
RUN apk add git && \
    go build ./cmd/x'
case_ "GOWORK=off on a continued ENV line" 0 Dockerfile 'FROM golang AS build
ENV CGO_ENABLED=0 \
    GOWORK=off
RUN go build ./cmd/x'
if "${root}/hack/check-dockerfiles.sh" "${root}" > /dev/null 2>&1; then echo "ok   this repository"; else echo "FAIL this repository"; fails=$((fails + 1)); fi

if [ "${fails}" -ne 0 ]; then
  echo "${fails} case(s) failed" >&2
  exit 1
fi
echo "all cases passed"
