FROM tvtamas/golang

RUN mkdir -p /go/src/announcer
WORKDIR /go/src/announcer
COPY . /go/src/announcer

RUN glide install -s -v
CMD ["/usr/local/go/bin/go", "run", "announcer.go"]
