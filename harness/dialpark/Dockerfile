FROM golang:1.25 AS build
WORKDIR /src
COPY server/ ./server/
# Deps are vendored, so no module network is needed.
RUN cd server && CGO_ENABLED=0 go build -mod=vendor -trimpath -o /dialpark-server ./cmd/dialpark-server \
 && CGO_ENABLED=0 go build -mod=vendor -trimpath -o /fake-app ./cmd/fake-app \
 && CGO_ENABLED=0 go build -mod=vendor -trimpath -o /dialpark-admin ./cmd/dialpark-admin
# /data is owned by distroless's nonroot uid so the named volume, which
# inherits the image directory's ownership on first mount, is writable.
RUN mkdir -p /data && chown 65532:65532 /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /dialpark-server /dialpark-server
COPY --from=build /fake-app /fake-app
COPY --from=build /dialpark-admin /dialpark-admin
COPY --chown=65532:65532 harness/admin/password /admin-password
COPY --from=build --chown=65532:65532 /data /data
# The harness's fixed install id and the dev licence issued for it (SPEC
# §4.9): baked into /data so a fresh named volume inherits them on first
# mount. provision.sh applies the licence through the admin API as well,
# which also covers a volume that predates licensing.
COPY --chown=65532:65532 harness/dialpark/install.id harness/dialpark/licence* /data/
VOLUME ["/data"]
EXPOSE 7443 5061 8080 8081 8443
ENTRYPOINT ["/dialpark-server"]
