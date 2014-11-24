FROM flynn/busybox

ADD https://raw.githubusercontent.com/bagder/ca-bundle/master/ca-bundle.crt /etc/ssl/certs/ca-certificates.crt
ADD https://github.com/coreos/etcd/releases/download/v0.5.0-alpha.4/etcd-v0.5.0-alpha.4-linux-amd64.tar.gz /tmp/etcd.tar.gz
RUN cd /tmp && \
    tar xzf etcd.tar.gz && \
    mv etcd-v0.5.0-alpha.4-linux-amd64/etcd etcd-v0.5.0-alpha.4-linux-amd64/etcdctl /bin && \
    rm -rf etcd.tar.gz etcd-v0.5.0-alpha.4-linux-amd64

ENTRYPOINT ["/bin/etcd"]
