package machinestatus

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/rancher/lasso/pkg/dynamic"
	"github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1/plan"
	capicontrollers "github.com/rancher/rancher/pkg/generated/controllers/cluster.x-k8s.io/v1alpha4"
	mgmtcontrollers "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	provisioningcontrollers "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io/v1"
	rkecontroller "github.com/rancher/rancher/pkg/generated/controllers/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/provisioningv2/rke2/planner"
	"github.com/rancher/rancher/pkg/wrangler"
	"github.com/rancher/wrangler/pkg/condition"
	"github.com/rancher/wrangler/pkg/data"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/relatedresource"
	corev1 "k8s.io/api/core/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	capi "sigs.k8s.io/cluster-api/api/v1alpha4"
)

const (
	Provisioned = condition.Cond("Provisioned")
)

type handler struct {
	secrets              corecontrollers.SecretCache
	machines             capicontrollers.MachineController
	bootstrapCache       rkecontroller.RKEBootstrapCache
	bootstrapController  rkecontroller.RKEBootstrapController
	capiClusterCache     capicontrollers.ClusterCache
	provClusterCache     provisioningcontrollers.ClusterCache
	mgmtClusterCache     mgmtcontrollers.ClusterCache
	rkeControlPlaneCache rkecontroller.RKEControlPlaneCache
	dynamic              *dynamic.Controller
}

func Register(ctx context.Context, clients *wrangler.Context) {
	h := handler{
		secrets:              clients.Core.Secret().Cache(),
		machines:             clients.CAPI.Machine(),
		bootstrapCache:       clients.RKE.RKEBootstrap().Cache(),
		bootstrapController:  clients.RKE.RKEBootstrap(),
		capiClusterCache:     clients.CAPI.Cluster().Cache(),
		mgmtClusterCache:     clients.Mgmt.Cluster().Cache(),
		provClusterCache:     clients.Provisioning.Cluster().Cache(),
		rkeControlPlaneCache: clients.RKE.RKEControlPlane().Cache(),
		dynamic:              clients.Dynamic,
	}
	clients.CAPI.Machine().OnChange(ctx, "machine-status", h.OnChange)

	relatedresource.Watch(ctx, "machine-trigger-from-secret", func(namespace, name string, obj runtime.Object) ([]relatedresource.Key, error) {
		if secret, ok := obj.(*corev1.Secret); ok {
			if secret.Type == planner.SecretTypeMachinePlan {
				return []relatedresource.Key{{
					Namespace: secret.Namespace,
					Name:      secret.Labels[planner.MachineNameLabel],
				}}, nil
			}
		}
		return nil, nil
	}, clients.CAPI.Machine(), clients.Core.Secret())

	h.dynamic.OnChange(ctx, "machine-trigger", func(gvk schema.GroupVersionKind) bool {
		return gvk.Group == "rke-machine.cattle.io"
	}, func(obj runtime.Object) (runtime.Object, error) {
		m, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		for _, owner := range m.GetOwnerReferences() {
			if owner.Kind == "Machine" {
				h.machines.Enqueue(m.GetNamespace(), owner.Name)
			}
		}
		return obj, nil
	})
}

func (h *handler) setJoinURLFromOutput(machine *capi.Machine, nodePlan *plan.Node) error {
	if nodePlan == nil || !planner.IsEtcdOnlyInitNode(machine) || machine.Annotations[planner.JoinURLAnnotation] != "" {
		return nil
	}

	address, ok := nodePlan.Output["capture-address"]
	if !ok {
		return nil
	}

	var str string
	scanner := bufio.NewScanner(bytes.NewBuffer(address))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "{") {
			str = line
			break
		}
	}

	if str == "" {
		return nil
	}

	dbInfo := &dbinfo{}
	if err := json.Unmarshal([]byte(str), dbInfo); err != nil {
		return err
	}

	if len(dbInfo.Members) == 0 {
		return nil
	}

	if len(dbInfo.Members[0].ClientURLs) == 0 {
		return nil
	}

	u, err := url.Parse(dbInfo.Members[0].ClientURLs[0])
	if err != nil {
		return err
	}

	cluster, err := h.capiClusterCache.Get(machine.Namespace, machine.Spec.ClusterName)
	if err != nil {
		return err
	}

	if cluster.Spec.ControlPlaneRef == nil {
		return nil
	}

	rkeControlPlane, err := h.rkeControlPlaneCache.Get(machine.Namespace, cluster.Spec.ControlPlaneRef.Name)
	if err != nil {
		return err
	}

	joinURL := fmt.Sprintf("https://%s:%d", u.Hostname(),
		planner.GetRuntimeSupervisorPort(rkeControlPlane.Spec.KubernetesVersion))
	machine = machine.DeepCopy()
	if machine.Annotations == nil {
		machine.Annotations = map[string]string{}
	}
	machine.Annotations[planner.JoinURLAnnotation] = joinURL
	_, err = h.machines.Update(machine)
	return err
}

func (h *handler) OnChange(key string, machine *capi.Machine) (*capi.Machine, error) {
	if machine == nil ||
		machine.Spec.Bootstrap.ConfigRef == nil ||
		machine.Spec.Bootstrap.ConfigRef.Kind != "RKEBootstrap" {
		return machine, nil
	}

	rkeBootstrap, err := h.bootstrapCache.Get(machine.Namespace, machine.Spec.Bootstrap.ConfigRef.Name)
	if err != nil {
		return machine, err
	}

	secret, err := h.secrets.Get(machine.Namespace, planner.PlanSecretFromBootstrapName(rkeBootstrap.Name))
	if apierror.IsNotFound(err) {
		// When the secret exists this handler will be triggered, so don't error
		return machine, nil
	} else if err != nil {
		return machine, err
	}

	plan, err := planner.SecretToNode(secret)
	if err != nil {
		return machine, err
	}

	if err := h.setJoinURLFromOutput(machine, plan); err != nil {
		return machine, err
	}

	status, reason, message, providerID, err := h.getInfraMachineState(machine)
	if err != nil {
		return machine, err
	}

	if status == "" {
		status, reason, message = planner.GetPlanStatusReasonMessage(machine, plan)
	}

	if status == corev1.ConditionTrue && providerID == "" {
		status = corev1.ConditionUnknown
		reason = "NoProviderID"
		message = "waiting for node to be registered in Kubernetes"
		provCluster, err := h.provClusterCache.Get(machine.Namespace, machine.Spec.ClusterName)
		if err == nil {
			mgmtCluster, err := h.mgmtClusterCache.Get(provCluster.Status.ClusterName)
			if err == nil {
				if condition.Cond("Ready").IsTrue(mgmtCluster) {
					h.bootstrapController.Enqueue(machine.Spec.Bootstrap.ConfigRef.Namespace, machine.Spec.Bootstrap.ConfigRef.Name)
				} else if planner.IsOnlyEtcd(machine) {
					message = "waiting for cluster agent to be available on a control plane node"
				} else {
					message = "waiting for cluster agent to be available"
				}
			}
		}
	}

	if corev1.ConditionStatus(Provisioned.GetStatus(machine)) != status ||
		Provisioned.GetReason(machine) != string(reason) ||
		Provisioned.GetMessage(machine) != message {
		machine := machine.DeepCopy()
		newCond := capi.Condition{
			Type:               capi.ConditionType(Provisioned),
			Status:             status,
			LastTransitionTime: metav1.Now(),
			Reason:             string(reason),
			Message:            message,
		}
		if status == corev1.ConditionFalse {
			newCond.Severity = capi.ConditionSeverityError
		} else {
			newCond.Severity = capi.ConditionSeverityInfo
		}

		set := false
		for i, cond := range machine.Status.Conditions {
			if string(cond.Type) == string(Provisioned) {
				set = true
				machine.Status.Conditions[i] = newCond
				break
			}
		}

		if !set {
			machine.Status.Conditions = append(machine.Status.Conditions, newCond)
		}

		_, err := h.machines.UpdateStatus(machine)
		if err != nil {
			return machine, err
		}
	}

	return machine, nil
}

func (h *handler) getInfraMachineState(capiMachine *capi.Machine) (status corev1.ConditionStatus, reason, message, providerID string, err error) {
	gvk := schema.FromAPIVersionAndKind(capiMachine.Spec.InfrastructureRef.APIVersion, capiMachine.Spec.InfrastructureRef.Kind)
	machine, err := h.dynamic.Get(gvk, capiMachine.Namespace, capiMachine.Spec.InfrastructureRef.Name)
	if apierror.IsNotFound(err) {
		return corev1.ConditionUnknown, "NoMachineDefined", "waiting for machine to be defined", "", nil
	} else if err != nil {
		return "", "", "", "", err
	}

	obj, err := data.Convert(machine)
	if err != nil {
		return "", "", "", "", err
	}

	if capiMachine.Spec.InfrastructureRef.APIVersion == "rke-machine.cattle.io/v1" {
		if obj.String("status", "jobName") == "" {
			return corev1.ConditionUnknown, "NoJob", "waiting to schedule machine create", "", nil
		}

		if !obj.Bool("status", "jobComplete") {
			return corev1.ConditionUnknown, "Creating",
				fmt.Sprintf("creating server (%s) in infrastructure provider", capiMachine.Spec.InfrastructureRef.Kind), "", nil
		}
	}

	return "", "", "", obj.String("spec", "providerID"), nil
}

type dbinfo struct {
	Members []member `json:"members,omitempty"`
}
type member struct {
	ClientURLs []string `json:"clientURLs,omitempty"`
}
