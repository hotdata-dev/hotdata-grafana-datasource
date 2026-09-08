# Build the mock Hotdata API used for local development, e2e tests, and
# plugin review. Build context is the repository root.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg ./pkg
RUN CGO_ENABLED=0 go build -o /mockapi ./pkg/cmd/mockapi

FROM alpine:3.22
COPY --from=build /mockapi /mockapi
EXPOSE 8999
ENTRYPOINT ["/mockapi"]
