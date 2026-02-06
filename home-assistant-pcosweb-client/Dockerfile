FROM golang:1.25-alpine AS builder

WORKDIR /app

ENV CGO_ENABLED=0
RUN env GOBIN=/app go install github.com/tgulacsi/pcosweb-client@master
RUN ls -lA /app

FROM alpine:latest AS runner

WORKDIR /app

COPY --from=builder /app/pcosweb-client .

EXPOSE 7070

CMD ["/app/pcosweb-client"]
