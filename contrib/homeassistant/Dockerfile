FROM golang:1.25-alpine AS builder

WORKDIR /app

RUN go install github.com/tgulacsi/pcosweb-client@latest
RUN cp -a $(go env GOBIN)/pcosweb /app/

FROM alpine:latest AS runner

WORKDIR /app

COPY --from=builder /app/pcosweb .

EXPOSE 7070

CMD ["./pcoweb"]
