# One image, two binaries: Cloud Run runs dispatch-mcp by default and
# fakefleet via --command=/fakefleet.
FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/dispatch-mcp ./cmd/dispatch-mcp \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fakefleet ./cmd/fakefleet

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /
USER nonroot
ENTRYPOINT ["/dispatch-mcp"]
