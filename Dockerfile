# ---- build stage ----
FROM golang:1.26-alpine AS build

WORKDIR /src

# tgpt is optional inside the container; the app degrades gracefully if missing.
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /app/ai-social-publisher ./cmd/server

# ---- runtime stage ----
FROM alpine:3.20

WORKDIR /app
RUN apk add --no-cache ca-certificates

COPY --from=build /app/ai-social-publisher /app/ai-social-publisher
COPY migrations /app/migrations
COPY templates /app/templates
COPY config.example.yaml /app/config.example.yaml

EXPOSE 8080

ENTRYPOINT ["/app/ai-social-publisher"]
