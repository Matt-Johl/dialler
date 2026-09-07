# Phase 0 engine-spike B2BUA (diago). Built from vendored deps so no module
# network is needed at build time.
FROM golang:1.25 AS build
WORKDIR /src
COPY spike/ ./spike/
RUN cd spike && CGO_ENABLED=0 go build -mod=vendor -trimpath -o /spike-b2bua .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /spike-b2bua /spike-b2bua
EXPOSE 5061
ENTRYPOINT ["/spike-b2bua"]
