FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /sablecourtd ./cmd/sablecourtd

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /sablecourtd /sablecourtd
EXPOSE 7432
ENTRYPOINT ["/sablecourtd"]
