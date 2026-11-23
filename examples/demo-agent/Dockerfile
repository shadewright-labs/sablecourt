FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY examples/demo-agent ./examples/demo-agent
RUN CGO_ENABLED=0 go build -o /demo-agent ./examples/demo-agent

FROM scratch
COPY --from=build /demo-agent /demo-agent
ENTRYPOINT ["/demo-agent"]
