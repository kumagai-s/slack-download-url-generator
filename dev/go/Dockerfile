FROM golang:1.19-alpine

RUN apk update && apk add git \
    gcc \
    musl-dev

#lsp server
RUN go install golang.org/x/tools/gopls@latest&& \
#hotreload
    go install github.com/cosmtrek/air@latest&& \
#delve for debug
    go install github.com/go-delve/delve/cmd/dlv@master
