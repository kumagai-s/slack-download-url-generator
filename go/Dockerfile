FROM golang:1.19-alpine

RUN apk update && apk add --no-cache sudo gcc musl-dev openssh git

RUN adduser -g vscode -D -u 1000 vscode vscode \
    && echo '%vscode ALL=(ALL) NOPASSWORD: ALL/g' >> /etc/sudoers

USER vscode

RUN go install golang.org/x/tools/gopls@latest \
    && go install github.com/cosmtrek/air@latest \
    && go install github.com/go-delve/delve/cmd/dlv@master