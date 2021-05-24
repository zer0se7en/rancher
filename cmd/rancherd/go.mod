module github.com/rancher/rancher/cmd/rancherd

go 1.14

replace (
	github.com/Microsoft/hcsshim => github.com/Microsoft/hcsshim v0.8.9
	github.com/benmoss/go-powershell => github.com/rancher/go-powershell v0.0.0-20200701184732-233247d45373
	github.com/containerd/btrfs => github.com/containerd/btrfs v0.0.0-20181101203652-af5082808c83
	github.com/containerd/cgroups => github.com/containerd/cgroups v0.0.0-20200531161412-0dbf7f05ba59
	github.com/containerd/console => github.com/containerd/console v0.0.0-20181022165439-0650fd9eeb50
	github.com/containerd/containerd => github.com/k3s-io/containerd v1.4.4-k3s1
	github.com/containerd/continuity => github.com/containerd/continuity v0.0.0-20190815185530-f2a389ac0a02
	github.com/containerd/fifo => github.com/containerd/fifo v0.0.0-20190816180239-bda0ff6ed73c
	github.com/containerd/go-runc => github.com/containerd/go-runc v0.0.0-20200220073739-7016d3ce2328
	github.com/containerd/typeurl => github.com/containerd/typeurl v0.0.0-20180627222232-a93fcdb778cd
	github.com/coreos/flannel => github.com/rancher/flannel v0.12.0-k3s1
	github.com/coreos/go-systemd => github.com/coreos/go-systemd v0.0.0-20190321100706-95778dfbb74e
	github.com/docker/distribution => github.com/docker/distribution v0.0.0-20190205005809-0d3efadf0154
	github.com/docker/docker => github.com/docker/docker v17.12.0-ce-rc1.0.20200310163718-4634ce647cf2+incompatible
	github.com/docker/libnetwork => github.com/docker/libnetwork v0.8.0-dev.2.0.20190624125649-f0e46a78ea34
	github.com/go-critic/go-critic => github.com/go-critic/go-critic v0.3.5-0.20190526074819-1df300866540
	github.com/golang/protobuf => github.com/golang/protobuf v1.3.5
	github.com/golangci/errcheck => github.com/golangci/errcheck v0.0.0-20181223084120-ef45e06d44b6
	github.com/golangci/go-tools => github.com/golangci/go-tools v0.0.0-20190318060251-af6baa5dc196
	github.com/golangci/gofmt => github.com/golangci/gofmt v0.0.0-20181222123516-0b8337e80d98
	github.com/golangci/gosec => github.com/golangci/gosec v0.0.0-20190211064107-66fb7fc33547
	github.com/golangci/ineffassign => github.com/golangci/ineffassign v0.0.0-20190609212857-42439a7714cc
	github.com/golangci/lint-1 => github.com/golangci/lint-1 v0.0.0-20190420132249-ee948d087217
	github.com/kubernetes-sigs/cri-tools => github.com/rancher/cri-tools v1.19.0-k3s1
	github.com/matryer/moq => github.com/rancher/moq v0.0.0-20190404221404-ee5226d43009
	github.com/opencontainers/runc => github.com/opencontainers/runc v1.0.0-rc92
	github.com/opencontainers/runtime-spec => github.com/opencontainers/runtime-spec v1.0.3-0.20200728170252-4d89ac9fbff6
	go.etcd.io/etcd => github.com/k3s-io/etcd v0.5.0-alpha.5.0.20201208200253-50621aee4aea
	google.golang.org/genproto => google.golang.org/genproto v0.0.0-20200224152610-e50cd9704f63
	google.golang.org/grpc => google.golang.org/grpc v1.27.1
	gopkg.in/square/go-jose.v2 => gopkg.in/square/go-jose.v2 v2.2.2

	k8s.io/api => github.com/k3s-io/kubernetes/staging/src/k8s.io/api v1.20.2-k3s1
	k8s.io/apiextensions-apiserver => github.com/k3s-io/kubernetes/staging/src/k8s.io/apiextensions-apiserver v1.20.2-k3s1
	k8s.io/apimachinery => github.com/k3s-io/kubernetes/staging/src/k8s.io/apimachinery v1.20.2-k3s1
	k8s.io/apiserver => github.com/k3s-io/kubernetes/staging/src/k8s.io/apiserver v1.20.2-k3s1
	k8s.io/cli-runtime => github.com/k3s-io/kubernetes/staging/src/k8s.io/cli-runtime v1.20.2-k3s1
	k8s.io/client-go => github.com/k3s-io/kubernetes/staging/src/k8s.io/client-go v1.20.2-k3s1
	k8s.io/cloud-provider => github.com/k3s-io/kubernetes/staging/src/k8s.io/cloud-provider v1.20.2-k3s1
	k8s.io/cluster-bootstrap => github.com/k3s-io/kubernetes/staging/src/k8s.io/cluster-bootstrap v1.20.2-k3s1
	k8s.io/code-generator => github.com/k3s-io/kubernetes/staging/src/k8s.io/code-generator v1.20.2-k3s1
	k8s.io/component-base => github.com/k3s-io/kubernetes/staging/src/k8s.io/component-base v1.20.2-k3s1
	k8s.io/component-helpers => github.com/k3s-io/kubernetes/staging/src/k8s.io/component-helpers v1.20.2-k3s1
	k8s.io/controller-manager => github.com/k3s-io/kubernetes/staging/src/k8s.io/controller-manager v1.20.2-k3s1
	k8s.io/cri-api => github.com/k3s-io/kubernetes/staging/src/k8s.io/cri-api v1.20.2-k3s1
	k8s.io/csi-translation-lib => github.com/k3s-io/kubernetes/staging/src/k8s.io/csi-translation-lib v1.20.2-k3s1
	k8s.io/kube-aggregator => github.com/k3s-io/kubernetes/staging/src/k8s.io/kube-aggregator v1.20.2-k3s1
	k8s.io/kube-controller-manager => github.com/k3s-io/kubernetes/staging/src/k8s.io/kube-controller-manager v1.20.2-k3s1
	k8s.io/kube-proxy => github.com/k3s-io/kubernetes/staging/src/k8s.io/kube-proxy v1.20.2-k3s1
	k8s.io/kube-scheduler => github.com/k3s-io/kubernetes/staging/src/k8s.io/kube-scheduler v1.20.2-k3s1
	k8s.io/kubectl => github.com/k3s-io/kubernetes/staging/src/k8s.io/kubectl v1.20.2-k3s1
	k8s.io/kubelet => github.com/k3s-io/kubernetes/staging/src/k8s.io/kubelet v1.20.2-k3s1
	k8s.io/kubernetes => github.com/k3s-io/kubernetes v1.20.2-k3s1
	k8s.io/legacy-cloud-providers => github.com/k3s-io/kubernetes/staging/src/k8s.io/legacy-cloud-providers v1.20.2-k3s1
	k8s.io/metrics => github.com/k3s-io/kubernetes/staging/src/k8s.io/metrics v1.20.2-k3s1
	k8s.io/mount-utils => github.com/k3s-io/kubernetes/staging/src/k8s.io/mount-utils v1.20.2-k3s1
	k8s.io/sample-apiserver => github.com/k3s-io/kubernetes/staging/src/k8s.io/sample-apiserver v1.20.2-k3s1
)

require (
	github.com/pkg/errors v0.9.1
	github.com/rancher/k3s v1.20.3-0.20210311183900-69f96d622580
	github.com/rancher/rke2 v1.20.6-0.20210402014303-e3d8dc388a5a // v1.20.5+rke2r1 ... If you update this, also update RKE2_VERSION in rancher/package/Dockerfile.runtime
	github.com/rancher/wrangler v0.6.1
	github.com/sirupsen/logrus v1.7.0
	github.com/urfave/cli v1.22.2
	golang.org/x/crypto v0.0.0-20201002170205-7f63de1d35b0
	gopkg.in/yaml.v2 v2.3.0
	k8s.io/api v0.19.0
	k8s.io/apimachinery v0.19.0
	k8s.io/client-go v11.0.1-0.20190409021438-1a26190bd76a+incompatible
)
