FROM golang:alpine as build
COPY ./ /app
WORKDIR /app
RUN go build

FROM alpine
COPY --from=build /app/elasticsearch-lm /elasticsearch-lm
CMD ./elasticsearch-lm