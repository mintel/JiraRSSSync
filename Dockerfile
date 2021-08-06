FROM golang:1.16.7-buster
RUN mkdir /app
COPY go.mod /app/
WORKDIR /app
RUN go mod download
COPY . /app
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o rss_sync .

COPY wait-for-it.sh /app/
RUN chmod +x /app/wait-for-it.sh
ENTRYPOINT ["/app/rss_sync"]