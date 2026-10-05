FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /relieflink ./cmd/relieflink

FROM alpine:3.20
RUN adduser -D -u 10001 relief
COPY --from=build /relieflink /usr/local/bin/relieflink
USER relief
EXPOSE 8080
ENTRYPOINT ["relieflink"]
CMD ["serve"]
