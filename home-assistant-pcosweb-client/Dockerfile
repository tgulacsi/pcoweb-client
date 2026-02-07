# ARG BUILD_FROM

FROM golang:1.25-alpine AS builder

WORKDIR /app

ENV CGO_ENABLED=0
RUN env GOBIN=/app go install github.com/tgulacsi/pcosweb-client@master
RUN ls -lA /app

# FROM $BUILD_FROM AS runner
FROM homeassistant/home-assistant AS runner

WORKDIR /app

COPY --from=builder /app/pcosweb-client .

EXPOSE 7070

CMD ["/app/pcosweb-client", "--addr=0.0.0.0:7070"]
