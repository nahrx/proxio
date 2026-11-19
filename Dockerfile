FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /proxio .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /proxio /usr/local/bin/proxio
EXPOSE 8080 8081
ENTRYPOINT ["proxio"]
CMD ["-addr", ":8080", "-forward", ":8081"]
