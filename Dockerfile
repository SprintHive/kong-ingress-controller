FROM golang:1.9.1-alpine as builder
RUN apk update
RUN apk add curl git gcc libc-dev
RUN curl https://glide.sh/get | sh
ADD . /go/src/github.com/SprintHive/kong-ingress-controller
WORKDIR /go/src/github.com/SprintHive/kong-ingress-controller
RUN glide install
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /kong-ingress-controller .
RUN mkdir /emptytmp

FROM scratch
COPY --from=builder /kong-ingress-controller /kong-ingress-controller
COPY --from=builder /emptytmp /tmp
ENTRYPOINT ["/kong-ingress-controller", "-alsologtostderr"]
