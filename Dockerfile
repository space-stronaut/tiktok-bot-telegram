FROM golang:1.23-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tiktok-bot .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/tiktok-bot /app/tiktok-bot

USER nobody
ENTRYPOINT ["/app/tiktok-bot"]
