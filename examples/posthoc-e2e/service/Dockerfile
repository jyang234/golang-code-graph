# Build the OTLP-exporting loansvc stand-in. The build context is this service
# directory only (its own module), so flowmap's engine deps never enter the image.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /loansvc-otlp .

FROM gcr.io/distroless/static-debian12
COPY --from=build /loansvc-otlp /loansvc-otlp
EXPOSE 8080
ENTRYPOINT ["/loansvc-otlp"]
