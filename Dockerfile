# ---- build stage ----
FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/untis-server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/untis-seed ./cmd/seed && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/untis-perm ./cmd/perm

# ---- runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/

VOLUME /data
ENV UNTIS_ADDR=:8509 \
    UNTIS_SERVER=schuldorf.webuntis.com \
    UNTIS_SCHOOL=schuldorf \
    UNTIS_DB=/data/untis.db

EXPOSE 8509
CMD ["sh", "-c", "untis-server -addr \"$UNTIS_ADDR\" -server \"$UNTIS_SERVER\" -school \"$UNTIS_SCHOOL\" -db \"$UNTIS_DB\""]
