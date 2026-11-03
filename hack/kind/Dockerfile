# Imagen rápida para pruebas locales con kind: el binario se compila en el host
# (make kind-build), así que no se descarga Go ni dependencias dentro de Docker.
FROM gcr.io/distroless/static:nonroot
COPY bin/manager-linux /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
