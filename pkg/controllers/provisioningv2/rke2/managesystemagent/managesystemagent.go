package managesystemagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	rancherv1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	v3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	rocontrollers "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io/v1"
	namespaces "github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/systemtemplate"
	"github.com/rancher/rancher/pkg/wrangler"
	upgradev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/gvk"
	"github.com/rancher/wrangler/pkg/name"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type handler struct {
	clusterRegistrationTokens v3.ClusterRegistrationTokenCache
}

func Register(ctx context.Context, clients *wrangler.Context) {
	h := &handler{
		clusterRegistrationTokens: clients.Mgmt.ClusterRegistrationToken().Cache(),
	}
	rocontrollers.RegisterClusterGeneratingHandler(ctx, clients.Provisioning.Cluster(),
		clients.Apply.
			WithSetOwnerReference(false, false).
			WithCacheTypes(clients.Fleet.Bundle(),
				clients.Provisioning.Cluster(),
				clients.Core.Secret(),
				clients.RBAC.RoleBinding(),
				clients.RBAC.Role()),
		"", "manage-system-agent", h.OnChange, &generic.GeneratingHandlerOptions{
			AllowCrossNamespace: true,
		})
	rocontrollers.RegisterClusterGeneratingHandler(ctx, clients.Provisioning.Cluster(),
		clients.Apply.
			WithSetOwnerReference(false, false).
			WithCacheTypes(clients.Mgmt.ManagedChart(),
				clients.Provisioning.Cluster()),
		"", "manage-system-upgrade-controller", h.OnChangeInstallSUC, nil)
}

func (h *handler) OnChange(cluster *rancherv1.Cluster, status rancherv1.ClusterStatus) ([]runtime.Object, rancherv1.ClusterStatus, error) {
	if cluster.Spec.RKEConfig == nil || settings.SystemAgentUpgradeImage.Get() == "" {
		return nil, status, nil
	}

	var (
		secretName = "steve-aggregation"
		result     []runtime.Object
	)

	if cluster.Status.ClusterName == "local" && cluster.Namespace == "fleet-local" {
		secretName += "-local-"

		token, err := h.clusterRegistrationTokens.Get(cluster.Status.ClusterName, "default-token")
		if err != nil {
			return nil, status, err
		}
		if token.Status.Token == "" {
			return nil, status, fmt.Errorf("token not yet generated for %s/%s", token.Namespace, token.Name)
		}

		digest := sha256.New()
		digest.Write([]byte(settings.InternalServerURL.Get()))
		digest.Write([]byte(token.Status.Token))
		digest.Write([]byte(systemtemplate.InternalCAChecksum()))
		d := digest.Sum(nil)
		secretName += hex.EncodeToString(d[:])[:12]

		result = append(result, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: namespaces.System,
			},
			Data: map[string][]byte{
				"CATTLE_SERVER":      []byte(settings.InternalServerURL.Get()),
				"CATTLE_TOKEN":       []byte(token.Status.Token),
				"CATTLE_CA_CHECKSUM": []byte(systemtemplate.InternalCAChecksum()),
			},
		})
	}

	resources, err := ToResources(installer(len(cluster.Spec.RKEConfig.NodeConfig) == 0, secretName))
	if err != nil {
		return nil, status, err
	}

	result = append(result, &v1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      name.SafeConcatName(cluster.Name, "managed", "system", "agent"),
		},
		Spec: v1alpha1.BundleSpec{
			BundleDeploymentOptions: v1alpha1.BundleDeploymentOptions{
				DefaultNamespace: namespaces.System,
			},
			Resources: resources,
			Targets: []v1alpha1.BundleTarget{
				{
					ClusterName: cluster.Name,
					ClusterSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "provisioning.cattle.io/unmanaged-system-agent",
								Operator: metav1.LabelSelectorOpDoesNotExist,
							},
						},
					},
				},
			},
		},
	})

	return result, status, nil
}

func installer(allWorkers bool, secretName string) []runtime.Object {
	image := strings.SplitN(settings.SystemAgentUpgradeImage.Get(), ":", 2)
	version := "latest"
	if len(image) == 2 {
		version = image[1]
	}

	env := []corev1.EnvVar{{
		Name:  "CATTLE_SERVER_QUERY",
		Value: "?internal=true",
	}}

	if allWorkers {
		env = append(env, corev1.EnvVar{
			Name:  "CATTLE_ROLE_WORKER",
			Value: "true",
		})
	}

	return []runtime.Object{
		&upgradev1.Plan{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Plan",
				APIVersion: "upgrade.cattle.io/v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "system-agent-upgrader",
				Namespace: namespaces.System,
			},
			Spec: upgradev1.PlanSpec{
				Concurrency: 10,
				Version:     version,
				Tolerations: []corev1.Toleration{{
					Operator: corev1.TolerationOpExists,
				}},
				NodeSelector:       &metav1.LabelSelector{},
				ServiceAccountName: "system-agent-upgrader",
				Upgrade: &upgradev1.ContainerSpec{
					Image: settings.PrefixPrivateRegistry(image[0]),
					Env:   env,
					EnvFrom: []corev1.EnvFromSource{{
						SecretRef: &corev1.SecretEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: secretName,
							},
						},
					}},
				},
			},
		},
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "system-agent-upgrader",
				Namespace: namespaces.System,
			},
		},
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-agent-upgrader",
			},
			Rules: []rbacv1.PolicyRule{{
				Verbs:     []string{"get"},
				APIGroups: []string{""},
				Resources: []string{"nodes"},
			}},
		},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: "system-agent-upgrader",
			},
			Subjects: []rbacv1.Subject{{
				Kind:      "ServiceAccount",
				Name:      "system-agent-upgrader",
				Namespace: namespaces.System,
			}},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "system-agent-upgrader",
			},
		},
	}
}

func ToResources(objs []runtime.Object) (result []v1alpha1.BundleResource, err error) {
	for _, obj := range objs {
		obj = obj.DeepCopyObject()
		if err := gvk.Set(obj); err != nil {
			return nil, fmt.Errorf("failed to set gvk: %w", err)
		}

		typeMeta, err := meta.TypeAccessor(obj)
		if err != nil {
			return nil, err
		}

		meta, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}

		data, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}

		digest := sha256.Sum256(data)
		filename := name.SafeConcatName(typeMeta.GetKind(), meta.GetNamespace(), meta.GetName(), hex.EncodeToString(digest[:])[:12]) + ".yaml"
		result = append(result, v1alpha1.BundleResource{
			Name:    filename,
			Content: string(data),
		})
	}
	return
}
