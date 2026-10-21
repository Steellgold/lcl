FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lcl .

FROM scratch
COPY --from=build /out/lcl /usr/local/bin/lcl
ENTRYPOINT ["/usr/local/bin/lcl", "serve"]
