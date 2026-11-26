# Base images are pinned by tag and digest; Dependabot (docker ecosystem) keeps both current.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
# The same workspace policy as CI: build this module alone, never a go.work that the context may hold.
ENV GOWORK=off
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/sablecourtd ./cmd/sablecourtd
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/sablecourt-agent ./cmd/sablecourt-agent

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS agent
COPY --from=build /out/sablecourt-agent /sablecourt-agent
ENTRYPOINT ["/sablecourt-agent"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS daemon
COPY --from=build /out/sablecourtd /sablecourtd
EXPOSE 7432
ENTRYPOINT ["/sablecourtd"]
