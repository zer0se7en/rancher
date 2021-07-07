package aks

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"time"

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/rancher/aks-operator/controller"
	aksv1 "github.com/rancher/aks-operator/pkg/apis/aks.cattle.io/v1"
	apimgmtv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/controllers/management/clusteroperator"
	"github.com/rancher/rancher/pkg/controllers/management/clusterupstreamrefresher"
	"github.com/rancher/rancher/pkg/controllers/management/rbac"
	"github.com/rancher/rancher/pkg/dialer"
	mgmtv3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/systemaccount"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/rancher/rancher/pkg/wrangler"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	aksAPIGroup         = "aks.cattle.io"
	aksV1               = "aks.cattle.io/v1"
	aksOperatorTemplate = "system-library-rancher-aks-operator"
	aksOperator         = "rancher-aks-operator"
	aksShortName        = "AKS"
	enqueueTime         = time.Second * 5
)

type aksOperatorController struct {
	clusteroperator.OperatorController
}

func Register(ctx context.Context, wContext *wrangler.Context, mgmtCtx *config.ManagementContext) {
	aksClusterConfigResource := schema.GroupVersionResource{
		Group:    aksAPIGroup,
		Version:  "v1",
		Resource: "aksclusterconfigs",
	}

	aksCCDynamicClient := mgmtCtx.DynamicClient.Resource(aksClusterConfigResource)
	e := &aksOperatorController{clusteroperator.OperatorController{
		ClusterEnqueueAfter:  wContext.Mgmt.Cluster().EnqueueAfter,
		SecretsCache:         wContext.Core.Secret().Cache(),
		TemplateCache:        wContext.Mgmt.CatalogTemplate().Cache(),
		ProjectCache:         wContext.Mgmt.Project().Cache(),
		AppLister:            mgmtCtx.Project.Apps("").Controller().Lister(),
		AppClient:            mgmtCtx.Project.Apps(""),
		NsClient:             mgmtCtx.Core.Namespaces(""),
		ClusterClient:        wContext.Mgmt.Cluster(),
		CatalogManager:       mgmtCtx.CatalogManager,
		SystemAccountManager: systemaccount.NewManager(mgmtCtx),
		DynamicClient:        aksCCDynamicClient,
		ClientDialer:         mgmtCtx.Dialer,
		Discovery:            wContext.K8s.Discovery(),
	}}

	wContext.Mgmt.Cluster().OnChange(ctx, "aks-operator-controller", e.onClusterChange)
}

func (e *aksOperatorController) onClusterChange(key string, cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	if cluster == nil || cluster.DeletionTimestamp != nil || cluster.Spec.AKSConfig == nil {
		return cluster, nil
	}

	// set driver name
	if cluster.Status.Driver == "" {
		cluster = cluster.DeepCopy()
		cluster.Status.Driver = apimgmtv3.ClusterDriverAKS
		return e.ClusterClient.Update(cluster)
	}

	cluster, err := e.CheckCrdReady(cluster, "aks")
	if err != nil {
		return cluster, err
	}

	// get aks Cluster Config, if it does not exist, create it
	aksClusterConfigDynamic, err := e.DynamicClient.Namespace(namespace.GlobalNamespace).Get(context.TODO(), cluster.Name, v1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return cluster, err
		}

		cluster, err = e.SetUnknown(cluster, apimgmtv3.ClusterConditionWaiting, "Waiting for API to be available")
		if err != nil {
			return cluster, err
		}

		aksClusterConfigDynamic, err = buildAKSCCCreateObject(cluster)
		if err != nil {
			return cluster, err
		}

		aksClusterConfigDynamic, err = e.DynamicClient.Namespace(namespace.GlobalNamespace).Create(context.TODO(), aksClusterConfigDynamic, v1.CreateOptions{})
		if err != nil {
			return cluster, err
		}
	}

	aksClusterConfigMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cluster.Spec.AKSConfig)
	if err != nil {
		return cluster, err
	}

	// check for changes between aks spec on cluster and the aks spec on the aksClusterConfig object
	if !reflect.DeepEqual(aksClusterConfigMap, aksClusterConfigDynamic.Object["spec"]) {
		logrus.Infof("change detected for cluster [%s], updating AKSClusterConfig", cluster.Name)
		return e.updateAKSClusterConfig(cluster, aksClusterConfigDynamic, aksClusterConfigMap)
	}

	// get aks Cluster Config's phase
	status, _ := aksClusterConfigDynamic.Object["status"].(map[string]interface{})
	phase, _ := status["phase"]
	failureMessage, _ := status["failureMessage"].(string)

	switch phase {
	case "creating":
		if cluster.Status.AKSStatus.UpstreamSpec == nil {
			return e.setInitialUpstreamSpec(cluster)
		}

		e.ClusterEnqueueAfter(cluster.Name, enqueueTime)
		if failureMessage == "" {
			logrus.Infof("waiting for cluster AKS [%s] to finish creating", cluster.Name)
			return e.SetUnknown(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		}
		logrus.Infof("waiting for cluster AKS [%s] create failure to be resolved", cluster.Name)
		return e.SetFalse(cluster, apimgmtv3.ClusterConditionProvisioned, failureMessage)
	case "active":
		if cluster.Spec.AKSConfig.Imported {
			if cluster.Status.AKSStatus.UpstreamSpec == nil {
				// non imported clusters will have already had upstream spec set
				return e.setInitialUpstreamSpec(cluster)
			}

			if apimgmtv3.ClusterConditionPending.IsUnknown(cluster) {
				cluster = cluster.DeepCopy()
				apimgmtv3.ClusterConditionPending.True(cluster)
				return e.ClusterClient.Update(cluster)
			}
		}

		cluster, err = e.SetTrue(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		if err != nil {
			return cluster, err
		}

		if cluster.Status.AKSStatus.RBACEnabled == nil {
			enabled, ok := status["rbacEnabled"].(bool)
			if ok {
				cluster = cluster.DeepCopy()
				cluster.Status.AKSStatus.RBACEnabled = &enabled
				return e.ClusterClient.Update(cluster)
			}
		}

		if cluster.Status.APIEndpoint == "" {
			return e.RecordCAAndAPIEndpoint(cluster)
		}

		if cluster.Status.AKSStatus.PrivateRequiresTunnel == nil &&
			to.Bool(cluster.Status.AKSStatus.UpstreamSpec.PrivateCluster) {
			// In this case, the API endpoint is private and it has not been determined if Rancher must tunnel to communicate with it.
			// Check to see if we can still use the control plane endpoint even though
			// the cluster has private-only access
			serviceToken, mustTunnel, err := e.generateSATokenWithPublicAPI(cluster)
			if err != nil {
				return cluster, err
			}
			if mustTunnel != nil {
				cluster = cluster.DeepCopy()
				cluster.Status.AKSStatus.PrivateRequiresTunnel = mustTunnel
				cluster.Status.ServiceAccountToken = serviceToken
				return e.ClusterClient.Update(cluster)
			}
		}

		if cluster.Status.ServiceAccountToken == "" {
			cluster, err = e.generateAndSetServiceAccount(cluster)
			if err != nil {
				var statusErr error
				if err == dialer.ErrAgentDisconnected {
					// In this case, the API endpoint is private and rancher is waiting for the import cluster command to be run.
					cluster, statusErr = e.SetUnknown(cluster, apimgmtv3.ClusterConditionWaiting, "waiting for cluster agent to be deployed")
					if statusErr == nil {
						e.ClusterEnqueueAfter(cluster.Name, enqueueTime)
					}
					return cluster, statusErr
				}
				cluster, statusErr = e.SetFalse(cluster, apimgmtv3.ClusterConditionWaiting,
					fmt.Sprintf("failed to communicate with cluster: %v", err))
				if statusErr != nil {
					return cluster, statusErr
				}
				return cluster, err
			}
		}

		cluster, err = e.recordAppliedSpec(cluster)
		if err != nil {
			return cluster, err
		}
		return e.SetTrue(cluster, apimgmtv3.ClusterConditionUpdated, "")
	case "updating":
		cluster, err = e.SetTrue(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		if err != nil {
			return cluster, err
		}

		e.ClusterEnqueueAfter(cluster.Name, enqueueTime)
		if failureMessage == "" {
			logrus.Infof("waiting for cluster AKS [%s] to update", cluster.Name)
			return e.SetUnknown(cluster, apimgmtv3.ClusterConditionUpdated, "")
		}
		logrus.Infof("waiting for cluster AKS [%s] update failure to be resolved", cluster.Name)
		return e.SetFalse(cluster, apimgmtv3.ClusterConditionUpdated, failureMessage)
	default:
		if cluster.Spec.AKSConfig.Imported {
			cluster, err = e.SetUnknown(cluster, apimgmtv3.ClusterConditionPending, "")
			if err != nil {
				return cluster, err
			}
			logrus.Infof("waiting for cluster import [%s] to start", cluster.Name)
		} else {
			logrus.Infof("waiting for cluster create [%s] to start", cluster.Name)
		}

		e.ClusterEnqueueAfter(cluster.Name, enqueueTime)
		if failureMessage == "" {
			if cluster.Spec.AKSConfig.Imported {
				cluster, err = e.SetUnknown(cluster, apimgmtv3.ClusterConditionPending, "")
				if err != nil {
					return cluster, err
				}
				logrus.Infof("waiting for cluster import [%s] to start", cluster.Name)
			} else {
				logrus.Infof("waiting for cluster create [%s] to start", cluster.Name)
			}
			return e.SetUnknown(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		}
		logrus.Infof("waiting for cluster AKS [%s] pre-create failure to be resolved", cluster.Name)
		return e.SetFalse(cluster, apimgmtv3.ClusterConditionProvisioned, failureMessage)
	}
}

func (e *aksOperatorController) setInitialUpstreamSpec(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	logrus.Infof("setting initial upstreamSpec on cluster [%s]", cluster.Name)
	upstreamSpec, err := clusterupstreamrefresher.BuildAKSUpstreamSpec(e.SecretsCache, cluster)
	if err != nil {
		return cluster, err
	}
	cluster = cluster.DeepCopy()
	cluster.Status.AKSStatus.UpstreamSpec = upstreamSpec
	return e.ClusterClient.Update(cluster)
}

// updateAKSClusterConfig updates the AKSClusterConfig object's spec with the cluster's AKSConfig if they are not equal..
func (e *aksOperatorController) updateAKSClusterConfig(cluster *mgmtv3.Cluster, aksClusterConfigDynamic *unstructured.Unstructured, spec map[string]interface{}) (*mgmtv3.Cluster, error) {
	list, err := e.DynamicClient.Namespace(namespace.GlobalNamespace).List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return cluster, err
	}
	selector := fields.OneTermEqualSelector("metadata.name", cluster.Name)
	w, err := e.DynamicClient.Namespace(namespace.GlobalNamespace).Watch(context.TODO(), v1.ListOptions{ResourceVersion: list.GetResourceVersion(), FieldSelector: selector.String()})
	if err != nil {
		return cluster, err
	}
	aksClusterConfigDynamic.Object["spec"] = spec
	aksClusterConfigDynamic, err = e.DynamicClient.Namespace(namespace.GlobalNamespace).Update(context.TODO(), aksClusterConfigDynamic, v1.UpdateOptions{})
	if err != nil {
		return cluster, err
	}

	// AKS cluster and node pool statuses are not always immediately updated. This cause the AKSConfig to
	// stay in "active" for a few seconds, causing the cluster to go back to "active".
	timeout := time.NewTimer(10 * time.Second)
	for {
		select {
		case event := <-w.ResultChan():
			aksClusterConfigDynamic = event.Object.(*unstructured.Unstructured)
			status, _ := aksClusterConfigDynamic.Object["status"].(map[string]interface{})
			if status["phase"] == "active" {
				continue
			}

			// this enqueue is necessary to ensure that the controller is reentered with the updating phase
			e.ClusterEnqueueAfter(cluster.Name, enqueueTime)
			return e.SetUnknown(cluster, apimgmtv3.ClusterConditionUpdated, "")
		case <-timeout.C:
			cluster, err = e.recordAppliedSpec(cluster)
			if err != nil {
				return cluster, err
			}
			return cluster, nil
		}
	}
}

// generateAndSetServiceAccount uses the API endpoint and CA cert to generate a service account token. The token is then copied to the cluster status.
func (e *aksOperatorController) generateAndSetServiceAccount(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	restConfig, err := e.getRestConfig(cluster)
	if err != nil {
		return cluster, fmt.Errorf("error getting service account token: %v", err)
	}

	clusterDialer, err := e.ClientDialer.ClusterDialer(cluster.Name)
	if err != nil {
		return cluster, err
	}

	restConfig.Dial = clusterDialer
	saToken, err := clusteroperator.GenerateSAToken(restConfig)
	if err != nil {
		return cluster, fmt.Errorf("error getting service account token: %v", err)
	}

	cluster = cluster.DeepCopy()
	cluster.Status.ServiceAccountToken = saToken
	return e.ClusterClient.Update(cluster)
}

// buildAKSCCCreateObject returns an object that can be used with the kubernetes dynamic client to
// create an AKSClusterConfig that matches the spec contained in the cluster's AKSConfig.
func buildAKSCCCreateObject(cluster *mgmtv3.Cluster) (*unstructured.Unstructured, error) {
	aksClusterConfig := aksv1.AKSClusterConfig{
		TypeMeta: v1.TypeMeta{
			Kind:       "AKSClusterConfig",
			APIVersion: aksV1,
		},
		ObjectMeta: v1.ObjectMeta{
			Name: cluster.Name,
			OwnerReferences: []v1.OwnerReference{
				{
					Kind:       cluster.Kind,
					APIVersion: rbac.RancherManagementAPIVersion,
					Name:       cluster.Name,
					UID:        cluster.UID,
				},
			},
		},
		Spec: *cluster.Spec.AKSConfig,
	}

	// convert AKS cluster config into unstructured object so it can be used with dynamic client
	aksClusterConfigMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&aksClusterConfig)
	if err != nil {
		return nil, err
	}

	return &unstructured.Unstructured{
		Object: aksClusterConfigMap,
	}, nil
}

// recordAppliedSpec sets the cluster's current spec as its appliedSpec
func (e *aksOperatorController) recordAppliedSpec(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	if reflect.DeepEqual(cluster.Status.AppliedSpec.AKSConfig, cluster.Spec.AKSConfig) {
		return cluster, nil
	}

	cluster = cluster.DeepCopy()
	cluster.Status.AppliedSpec.AKSConfig = cluster.Spec.AKSConfig
	return e.ClusterClient.Update(cluster)
}

// generateSATokenWithPublicAPI tries to get a service account token from the cluster using the public API endpoint.
// This function is called if the cluster has only privateEndpoint enabled and is not publicly available.
// If Rancher is able to communicate with the cluster through its API endpoint even though it is private, then this function will retrieve
// a service account token and the *bool returned will refer to a false value (doesn't have to tunnel).
//
// If the Rancher server cannot connect to the cluster's API endpoint, then one of the two errors below will happen.
// In this case, we know that Rancher must use the cluster agent tunnel for communication. This function will return an empty service account token,
// and the *bool return value will refer to a true value (must tunnel).
//
// If an error different from the two below occur, then the *bool return value will be nil, indicating that Rancher was not able to determine if
// tunneling is required to communicate with the cluster.
func (e *aksOperatorController) generateSATokenWithPublicAPI(cluster *mgmtv3.Cluster) (string, *bool, error) {
	restConfig, err := e.getRestConfig(cluster)
	if err != nil {
		return "", nil, err
	}

	requiresTunnel := new(bool)
	restConfig.Dial = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	serviceToken, err := clusteroperator.GenerateSAToken(restConfig)
	if err != nil {
		*requiresTunnel = true
		var dnsError *net.DNSError
		if stderrors.As(err, &dnsError) && !dnsError.IsTemporary {
			return "", requiresTunnel, nil
		}

		// In the existence of a proxy, it may be the case that the following error occurs,
		// in which case rancher should use the tunnel connection to communicate with the cluster.
		var urlError *url.Error
		if stderrors.As(err, &urlError) && urlError.Timeout() {
			return "", requiresTunnel, nil
		}

		// Not able to determine if tunneling is required.
		requiresTunnel = nil
	}

	return serviceToken, requiresTunnel, err
}

func (e *aksOperatorController) getRestConfig(cluster *mgmtv3.Cluster) (*rest.Config, error) {
	ctx := context.Background()
	restConfig, err := controller.GetClusterKubeConfig(ctx, e.SecretsCache, cluster.Spec.AKSConfig)
	if err != nil {
		return nil, err
	}
	return restConfig, nil
}
