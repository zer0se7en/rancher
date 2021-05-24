package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	capicontrollers "github.com/rancher/rancher/pkg/generated/controllers/cluster.x-k8s.io/v1alpha4"
	rkecontroller "github.com/rancher/rancher/pkg/generated/controllers/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/provisioningv2/rke2/installer"
	"github.com/rancher/rancher/pkg/provisioningv2/rke2/planner"
	"github.com/rancher/rancher/pkg/wrangler"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/name"
	"github.com/rancher/wrangler/pkg/relatedresource"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	capi "sigs.k8s.io/cluster-api/api/v1alpha4"
)

const (
	ClusterNameLabel = "rke.cattle.io/cluster-name"
	planSecret       = "rke.cattle.io/plan-secret-name"
	roleLabel        = "rke.cattle.io/service-account-role"
	rkeBootstrapName = "rke.cattle.io/rkebootstrap-name"
	roleBootstrap    = "bootstrap"
	rolePlan         = "plan"
)

var (
	bootstrapAPIVersion = fmt.Sprintf("%s/%s", rkev1.SchemeGroupVersion.Group, rkev1.SchemeGroupVersion.Version)
)

type handler struct {
	serviceAccountCache corecontrollers.ServiceAccountCache
	secretCache         corecontrollers.SecretCache
	machineCache        capicontrollers.MachineCache
	capiClusters        capicontrollers.ClusterCache
	rkeControlPlanes    rkecontroller.RKEControlPlaneCache
}

func Register(ctx context.Context, clients *wrangler.Context) {
	h := &handler{
		serviceAccountCache: clients.Core.ServiceAccount().Cache(),
		secretCache:         clients.Core.Secret().Cache(),
		machineCache:        clients.CAPI.Machine().Cache(),
		capiClusters:        clients.CAPI.Cluster().Cache(),
		rkeControlPlanes:    clients.RKE.RKEControlPlane().Cache(),
	}
	rkecontroller.RegisterRKEBootstrapGeneratingHandler(ctx,
		clients.RKE.RKEBootstrap(),
		clients.Apply.
			WithCacheTypes(
				clients.RBAC.Role(),
				clients.RBAC.RoleBinding(),
				clients.CAPI.Machine(),
				clients.Core.ServiceAccount(),
				clients.Core.Secret()).
			WithSetOwnerReference(true, true),
		"",
		"rke-machine",
		h.OnChange,
		nil)

	relatedresource.Watch(ctx, "rke-machine-trigger", func(namespace, name string, obj runtime.Object) ([]relatedresource.Key, error) {
		if sa, ok := obj.(*corev1.ServiceAccount); ok {
			if name, ok := sa.Labels[rkeBootstrapName]; ok {
				return []relatedresource.Key{
					{
						Namespace: sa.Namespace,
						Name:      name,
					},
				}, nil
			}
		}
		if machine, ok := obj.(*capi.Machine); ok {
			if machine.Spec.Bootstrap.ConfigRef != nil && machine.Spec.Bootstrap.ConfigRef.Kind == "RKEBootstrap" {
				return []relatedresource.Key{{
					Namespace: machine.Namespace,
					Name:      machine.Spec.Bootstrap.ConfigRef.Name,
				}}, nil
			}
		}
		return nil, nil
	}, clients.RKE.RKEBootstrap(), clients.Core.ServiceAccount(), clients.CAPI.Machine())
}

func (h *handler) getBootstrapSecret(namespace, name string, envVars []corev1.EnvVar) (*corev1.Secret, error) {
	sa, err := h.serviceAccountCache.Get(namespace, name)
	if apierror.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err

	}
	for _, secretRef := range sa.Secrets {
		secret, err := h.secretCache.Get(sa.Namespace, secretRef.Name)
		if err != nil {
			return nil, err
		}

		hash := sha256.Sum256(secret.Data["token"])
		data, err := installer.InstallScript(base64.URLEncoding.EncodeToString(hash[:]), envVars)
		if err != nil {
			return nil, err
		}

		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Data: map[string][]byte{
				"value": data,
			},
			Type: "rke.cattle.io/bootstrap",
		}, nil
	}

	return nil, nil
}

func (h *handler) assignPlanSecret(machine *capi.Machine, obj *rkev1.RKEBootstrap) ([]runtime.Object, error) {
	secretName := planner.PlanSecretFromBootstrapName(obj.Name)

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: obj.Namespace,
			Labels: map[string]string{
				planner.MachineNameLabel: machine.Name,
				rkeBootstrapName:         obj.Name,
				roleLabel:                rolePlan,
				planSecret:               secretName,
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: obj.Namespace,
			Labels: map[string]string{
				planner.MachineNameLabel: machine.Name,
			},
		},
		Type: planner.SecretTypeMachinePlan,
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: obj.Namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:         []string{"watch", "get", "update", "list"},
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				ResourceNames: []string{secretName},
			},
		},
	}
	rolebinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: obj.Namespace,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      sa.Name,
				Namespace: sa.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     secretName,
		},
	}

	return []runtime.Object{sa, secret, role, rolebinding}, nil
}

func (h *handler) getMachine(obj *rkev1.RKEBootstrap) (*capi.Machine, error) {
	for _, ref := range obj.OwnerReferences {
		gvk := schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)
		if capi.GroupVersion.Group != gvk.Group ||
			ref.Kind != "Machine" {
			continue
		}

		return h.machineCache.Get(obj.Namespace, ref.Name)
	}
	return nil, generic.ErrSkip
}

func (h *handler) getEnvVar(machine *capi.Machine) ([]corev1.EnvVar, error) {
	capiCluster, err := h.capiClusters.Get(machine.Namespace, machine.Spec.ClusterName)
	if apierror.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	if capiCluster.Spec.ControlPlaneRef == nil || capiCluster.Spec.ControlPlaneRef.Kind != "RKEControlPlane" {
		return nil, nil
	}

	cp, err := h.rkeControlPlanes.Get(machine.Namespace, capiCluster.Spec.ControlPlaneRef.Name)
	if err != nil {
		return nil, err
	}

	return cp.Spec.AgentEnvVars, nil
}

func (h *handler) assignBootStrapSecret(machine *capi.Machine, obj *rkev1.RKEBootstrap) (*corev1.Secret, []runtime.Object, error) {
	if capi.MachinePhase(machine.Status.Phase) != capi.MachinePhasePending &&
		capi.MachinePhase(machine.Status.Phase) != capi.MachinePhaseDeleting &&
		capi.MachinePhase(machine.Status.Phase) != capi.MachinePhaseFailed &&
		capi.MachinePhase(machine.Status.Phase) != capi.MachinePhaseProvisioning {
		return nil, nil, nil
	}

	envVars, err := h.getEnvVar(machine)
	if err != nil {
		return nil, nil, err
	}

	secretName := name.SafeConcatName(obj.Name, "machine", "bootstrap")

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: obj.Namespace,
			Labels: map[string]string{
				planner.MachineNameLabel: machine.Name,
				rkeBootstrapName:         obj.Name,
				roleLabel:                roleBootstrap,
			},
		},
	}

	bootstrapSecret, err := h.getBootstrapSecret(sa.Namespace, sa.Name, envVars)
	if err != nil {
		return nil, nil, err
	}

	return bootstrapSecret, []runtime.Object{sa}, nil
}

func (h *handler) OnChange(obj *rkev1.RKEBootstrap, status rkev1.RKEBootstrapStatus) ([]runtime.Object, rkev1.RKEBootstrapStatus, error) {
	var (
		result []runtime.Object
	)

	machine, err := h.getMachine(obj)
	if err != nil {
		return nil, status, err
	}

	objs, err := h.assignPlanSecret(machine, obj)
	if err != nil {
		return nil, status, err
	}

	result = append(result, objs...)

	bootstrapSecret, objs, err := h.assignBootStrapSecret(machine, obj)
	if err != nil {
		return nil, status, err
	}

	if bootstrapSecret != nil {
		if status.DataSecretName == nil {
			status.DataSecretName = &bootstrapSecret.Name
			status.Ready = true
		}
		result = append(result, bootstrapSecret)
	}

	result = append(result, objs...)
	return result, status, nil
}
