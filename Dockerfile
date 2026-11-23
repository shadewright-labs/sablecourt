FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/sablecourtd ./cmd/sablecourtd
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/sablecourt-agent ./cmd/sablecourt-agent

FROM gcr.io/distroless/static-debian12:nonroot AS agent
COPY --from=build /out/sablecourt-agent /sablecourt-agent
ENTRYPOINT ["/sablecourt-agent"]

FROM gcr.io/distroless/static-debian12:nonroot AS daemon
COPY --from=build /out/sablecourtd /sablecourtd
EXPOSE 7432
ENTRYPOINT ["/sablecourtd"]
