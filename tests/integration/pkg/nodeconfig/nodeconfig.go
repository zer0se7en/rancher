package nodeconfig

import (
	"encoding/json"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/tests/integration/pkg/clients"
	"github.com/rancher/rancher/tests/integration/pkg/defaults"
	"github.com/rancher/rancher/tests/integration/pkg/wait"
	"github.com/rancher/wrangler/pkg/condition"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func FromNode(node *corev1.Node) ([]string, error) {
	var args []string
	str := node.Annotations["rke2.io/node-args"]
	if str == "" {
		str = node.Annotations["k3s.io/node-args"]
	}

	return args, json.Unmarshal([]byte(str), &args)
}

func NewPodConfig(clients *clients.Clients, namespace string) (*corev1.ObjectReference, error) {
	_, err := clients.RBAC.Role().Create(&rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rke2-machine-provisioner",
			Namespace: namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"*"},
				APIGroups: []string{"*"},
				Resources: []string{"*"},
			},
		},
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	clients.OnClose(func() {
		_ = clients.RBAC.Role().Delete(namespace, "rke2-machine-provisioner", nil)
	})

	_, err = clients.Mgmt.NodeDriver().Create(&v3.NodeDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod",
		},
		Spec: v3.NodeDriverSpec{
			DisplayName: "pod",
			URL:         "local://",
			Builtin:     true,
			Active:      true,
		},
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}

	err = wait.ClusterScopedList(clients.Ctx, clients.CRD.CustomResourceDefinition().Watch, func(obj runtime.Object) (bool, error) {
		crd := obj.(*v1.CustomResourceDefinition)
		return crd.Name == "podconfigs.rke-machine-config.cattle.io" && condition.Cond("Established").IsTrue(crd), nil
	})
	if err != nil {
		return nil, err
	}

	podConfig := &unstructured.Unstructured{}
	podConfig.SetAPIVersion("rke-machine-config.cattle.io/v1")
	podConfig.SetKind("PodConfig")
	podConfig.SetNamespace(namespace)
	podConfig.SetGenerateName("pod-config-")
	podConfig.Object["image"] = defaults.PodTestImage
	podConfigClient := clients.Dynamic.Resource(schema.GroupVersionResource{
		Group:    "rke-machine-config.cattle.io",
		Version:  "v1",
		Resource: "podconfigs",
	})
	result, err := podConfigClient.Namespace(namespace).Create(clients.Ctx, podConfig, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	clients.OnClose(func() {
		_ = podConfigClient.Delete(clients.Ctx, result.GetName(), metav1.DeleteOptions{})
	})

	return &corev1.ObjectReference{
		Kind: result.GetKind(),
		Name: result.GetName(),
	}, nil
}
