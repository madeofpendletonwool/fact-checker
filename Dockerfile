# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/fact-checker ./cmd/fact-checker

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 app
COPY --from=build /out/fact-checker /usr/local/bin/fact-checker
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/fact-checker"]
CMD ["serve"]
