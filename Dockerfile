FROM busybox
MAINTAINER Salvador Girones <salvadorgirones@gmail.com>
COPY kube2consul /kube2consul
ENTRYPOINT ["/kube2consul"]
