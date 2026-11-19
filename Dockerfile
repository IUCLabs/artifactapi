FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o artifactapi ./cmd/artifactapi

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /build/artifactapi /usr/local/bin/artifactapi

EXPOSE 8000

ENTRYPOINT ["artifactapi"]
