FROM golang:1.18-bullseye as build
WORKDIR $GOPATH/src/github.com/michaelhenkel/virtual-kubelet
COPY . .
RUN go mod tidy
RUN go build -o /virtual-kubelet cmd/virtual-kubelet/main.go

FROM gcr.io/distroless/base-debian11:debug
COPY --from=build /virtual-kubelet /
COPY cni /tmp
