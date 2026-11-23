FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/sablecourt-agent ./cmd/sablecourt-agent
RUN CGO_ENABLED=0 go build -o /sablecourt-agent ./cmd/sablecourt-agent

FROM scratch
COPY --from=build /sablecourt-agent /sablecourt-agent
ENTRYPOINT ["/sablecourt-agent"]
