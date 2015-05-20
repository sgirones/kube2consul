.PHONY: all kube2consul container clean test

all: container

kube2consul: kube2consul.go
	CGO_ENABLED=0 godep go build -a -installsuffix cgo --ldflags '-w' ./kube2consul.go

container: kube2consul
	docker build -t kube2consul .

clean:
	rm -f kube2consul

test: clean
	godep go test -v --vmodule=*=4
