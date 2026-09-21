FROM docker.io/golang:1-alpine AS build

RUN apk add make

WORKDIR /build/agwtools
COPY go.* ./
RUN go mod download
COPY . ./
# The Makefile picks these up from the environment (there is no git repository
# in the build context to derive them from).
ARG VERSION
ARG COMMIT
RUN CGO_ENABLED=0 make && make prefix=/usr/local install

FROM docker.io/alpine:3

COPY --from=build /usr/local/bin/* /usr/local/bin/
