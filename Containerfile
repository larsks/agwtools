FROM docker.io/golang:1-alpine AS build

RUN apk add make

WORKDIR /build/agwtools
COPY go.* ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 make && make prefix=/usr/local install

FROM docker.io/alpine:3

COPY --from=build /usr/local/bin/* /usr/local/bin/
