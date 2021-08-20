FROM registry.ci.openshift.org/openshift/release:golang-1.15 AS builder
WORKDIR /go/src/github.com/mfojtik/bugzilla-operator
COPY . .
ENV GO_PACKAGE github.com/mfojtik/bugzilla-operator
RUN make build --warn-undefined-variables

FROM centos:8
RUN dnf install -y libvirt-libs
COPY --from=builder /go/src/github.com/mfojtik/bugzilla-operator/bugzilla-operator /usr/bin/
COPY --from=builder /go/src/github.com/mfojtik/bugzilla-operator/shared-cluster/create-cluster /usr/bin/create-cluster
COPY --from=builder /go/src/github.com/mfojtik/bugzilla-operator/shared-cluster/destroy-cluster /usr/bin/destroy-cluster
COPY --from=registry.ci.openshift.org/ocp/4.7:cli /bin/oc /usr/bin/
