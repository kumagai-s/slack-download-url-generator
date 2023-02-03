FROM golang:1.19-alpine

RUN adduser -g vscode -D -u 1000 vscode vscode

RUN apk update && apk add --no-cache gcc musl-dev git openssh

RUN go install golang.org/x/tools/gopls@latest \
    && go install github.com/cosmtrek/air@latest \
    && go install github.com/go-delve/delve/cmd/dlv@master

CMD ["air", "-c", "/workspace/.air.toml"]