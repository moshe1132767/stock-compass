# מצפן המניות — multi-stage build (בסגנון ReceiptKeeper: golang:1.25-alpine -> alpine:3.19)
FROM golang:1.25-alpine AS builder

WORKDIR /app

# תלויות קודם (caching) — אין תלויות חיצוניות, הכל ספריית סטנדרט
COPY go.mod ./
RUN go mod download

# קוד
COPY . .

# בנייה סטטית (אין CGO — אין SQLite)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o stockcompass ./cmd/stockcompass

# שלב ריצה
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/stockcompass .
COPY --from=builder /app/web ./web

ENV PORT=3003
ENV WEB_DIR=/app/web

EXPOSE 3003

CMD ["./stockcompass"]
