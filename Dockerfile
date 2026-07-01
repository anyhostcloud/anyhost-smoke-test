FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server .

FROM alpine:3.20
RUN adduser -D -H -u 10001 appuser
USER appuser
WORKDIR /app
COPY --from=build /out/server /app/server
EXPOSE 8080
CMD ["/app/server"]
