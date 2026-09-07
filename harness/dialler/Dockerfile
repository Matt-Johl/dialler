FROM golang:1.25 AS build
WORKDIR /src
COPY server/ ./server/
# Deps are vendored, so no module network is needed.
RUN cd server && CGO_ENABLED=0 go build -mod=vendor -trimpath -o /dialler-server ./cmd/dialler-server \
 && CGO_ENABLED=0 go build -mod=vendor -trimpath -o /fake-app ./cmd/fake-app
# /data is owned by distroless's nonroot uid so the named volume, which
# inherits the image directory's ownership on first mount, is writable.
RUN mkdir -p /data && chown 65532:65532 /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /dialler-server /dialler-server
COPY --from=build /fake-app /fake-app
COPY --from=build --chown=65532:65532 /data /data
VOLUME ["/data"]
EXPOSE 7443 5061 8080
ENTRYPOINT ["/dialler-server"]
