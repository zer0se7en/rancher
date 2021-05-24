package eks

import (
	"context"
	"encoding/base64"
	stderrors "errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/rancher/eks-operator/controller"
	eksv1 "github.com/rancher/eks-operator/pkg/apis/eks.cattle.io/v1"
	apimgmtv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/controllers/management/clusteroperator"
	"github.com/rancher/rancher/pkg/controllers/management/clusterupstreamrefresher"
	"github.com/rancher/rancher/pkg/controllers/management/rbac"
	"github.com/rancher/rancher/pkg/dialer"
	mgmtv3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/kontainer-engine/drivers/util"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/systemaccount"
	"github.com/rancher/rancher/pkg/types/config"
	typesDialer "github.com/rancher/rancher/pkg/types/config/dialer"
	"github.com/rancher/rancher/pkg/wrangler"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
)

const (
	eksAPIGroup         = "eks.cattle.io"
	eksV1               = "eks.cattle.io/v1"
	eksOperatorTemplate = "system-library-rancher-eks-operator"
	eksOperator         = "rancher-eks-operator"
	eksShortName        = "EKS"
	enqueueTime         = time.Second * 5
	importedAnno        = "eks.cattle.io/imported"
)

type eksOperatorController struct {
	clusteroperator.OperatorController
}

func Register(ctx context.Context, wContext *wrangler.Context, mgmtCtx *config.ManagementContext) {
	eksClusterConfigResource := schema.GroupVersionResource{
		Group:    eksAPIGroup,
		Version:  "v1",
		Resource: "eksclusterconfigs",
	}

	eksCCDynamicClient := mgmtCtx.DynamicClient.Resource(eksClusterConfigResource)
	e := &eksOperatorController{clusteroperator.OperatorController{
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
		DynamicClient:        eksCCDynamicClient,
		ClientDialer:         mgmtCtx.Dialer,
	}}

	wContext.Mgmt.Cluster().OnChange(ctx, "eks-operator-controller", e.onClusterChange)
}

func (e *eksOperatorController) onClusterChange(key string, cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	if cluster == nil || cluster.DeletionTimestamp != nil {
		return cluster, nil
	}

	if cluster.Spec.EKSConfig == nil {
		return cluster, nil
	}

	if err := e.deployEKSOperator(); err != nil {
		failedToDeployEKSOperatorErr := "failed to deploy eks-operator: %v"
		var conditionErr error
		if cluster.Spec.EKSConfig.Imported {
			cluster, conditionErr = e.SetFalse(cluster, apimgmtv3.ClusterConditionPending, fmt.Sprintf(failedToDeployEKSOperatorErr, err))
			if conditionErr != nil {
				return cluster, conditionErr
			}
		} else {
			cluster, conditionErr = e.SetFalse(cluster, apimgmtv3.ClusterConditionProvisioned, fmt.Sprintf(failedToDeployEKSOperatorErr, err))
			if conditionErr != nil {
				return cluster, conditionErr
			}
		}
		return cluster, err
	}

	// set driver name
	if cluster.Status.Driver == "" {
		cluster = cluster.DeepCopy()
		cluster.Status.Driver = apimgmtv3.ClusterDriverEKS
		var err error
		cluster, err = e.ClusterClient.Update(cluster)
		if err != nil {
			return cluster, err
		}
	}

	// get EKS Cluster Config, if it does not exist, create it
	eksClusterConfigDynamic, err := e.DynamicClient.Namespace(namespace.GlobalNamespace).Get(context.TODO(), cluster.Name, v1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return cluster, err
		}

		cluster, err = e.SetUnknown(cluster, apimgmtv3.ClusterConditionWaiting, "Waiting for API to be available")
		if err != nil {
			return cluster, err
		}

		eksClusterConfigDynamic, err = buildEKSCCCreateObject(cluster)
		if err != nil {
			return cluster, err
		}

		eksClusterConfigDynamic, err = e.DynamicClient.Namespace(namespace.GlobalNamespace).Create(context.TODO(), eksClusterConfigDynamic, v1.CreateOptions{})
		if err != nil {
			return cluster, err
		}

	}

	eksClusterConfigMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&cluster.Spec.EKSConfig)
	if err != nil {
		return cluster, err
	}

	// check for changes between EKS spec on cluster and the EKS spec on the EKSClusterConfig object
	if !reflect.DeepEqual(eksClusterConfigMap, eksClusterConfigDynamic.Object["spec"]) {
		logrus.Infof("change detected for cluster [%s], updating EKSClusterConfig", cluster.Name)
		return e.updateEKSClusterConfig(cluster, eksClusterConfigDynamic, eksClusterConfigMap)
	}

	// get EKS Cluster Config's phase
	status, _ := eksClusterConfigDynamic.Object["status"].(map[string]interface{})
	phase, _ := status["phase"]
	failureMessage, _ := status["failureMessage"].(string)
	if strings.Contains(failureMessage, "403") {
		failureMessage = fmt.Sprintf("cannot access EKS, check cloud credential: %s", failureMessage)
	}
	switch phase {
	case "creating":
		if cluster.Status.EKSStatus.UpstreamSpec == nil {
			cluster, err = e.setInitialUpstreamSpec(cluster)
			if err != nil {
				if !notFound(err) {
					return cluster, err
				}
			}
			return cluster, nil
		}

		e.ClusterEnqueueAfter(cluster.Name, enqueueTime)
		if failureMessage == "" {
			logrus.Infof("waiting for cluster EKS [%s] to finish creating", cluster.Name)
			return e.SetUnknown(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		}
		logrus.Infof("waiting for cluster EKS [%s] create failure to be resolved", cluster.Name)
		return e.SetFalse(cluster, apimgmtv3.ClusterConditionProvisioned, failureMessage)
	case "active":
		if cluster.Spec.EKSConfig.Imported {
			if cluster.Status.EKSStatus.UpstreamSpec == nil {
				// non imported clusters will have already had upstream spec set
				return e.setInitialUpstreamSpec(cluster)
			}

			if apimgmtv3.ClusterConditionPending.IsUnknown(cluster) {
				cluster = cluster.DeepCopy()
				apimgmtv3.ClusterConditionPending.True(cluster)
				cluster, err = e.ClusterClient.Update(cluster)
				if err != nil {
					return cluster, err
				}
			}
		}

		if apimgmtv3.ClusterConditionUpdated.IsFalse(cluster) && strings.HasPrefix(apimgmtv3.ClusterConditionUpdated.GetMessage(cluster), "[Syncing error") {
			return cluster, fmt.Errorf(apimgmtv3.ClusterConditionUpdated.GetMessage(cluster))
		}

		// cluster must have at least one managed nodegroup. It is possible for a cluster
		// agent to be deployed without one, but having a managed nodegroup makes it easy
		// for rancher to validate its ability to do so.
		addNgMessage := "Cluster must have at least one managed nodegroup."
		noNodeGroupsOnSpec := len(cluster.Spec.EKSConfig.NodeGroups) == 0
		noNodeGroupsOnUpstreamSpec := len(cluster.Status.EKSStatus.UpstreamSpec.NodeGroups) == 0
		if (cluster.Spec.EKSConfig.NodeGroups != nil && noNodeGroupsOnSpec) || (cluster.Spec.EKSConfig.NodeGroups == nil && noNodeGroupsOnUpstreamSpec) {
			cluster, err = e.SetFalse(cluster, apimgmtv3.ClusterConditionWaiting, addNgMessage)
			if err != nil {
				return cluster, err
			}
		} else {
			if apimgmtv3.ClusterConditionWaiting.GetMessage(cluster) == addNgMessage {
				cluster = cluster.DeepCopy()
				apimgmtv3.ClusterConditionWaiting.Message(cluster, "Waiting for API to be available")
				cluster, err = e.ClusterClient.Update(cluster)
				if err != nil {
					return cluster, err
				}
			}
		}

		cluster, err = e.SetTrue(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		if err != nil {
			return cluster, err
		}

		// If there are no subnets it can be assumed that networking fields are not provided. In which case they
		// should be created by the eks-operator, and needs to be copied to the cluster object.
		if len(cluster.Status.EKSStatus.Subnets) == 0 {
			subnets, _ := status["subnets"].([]interface{})
			if len(subnets) != 0 {
				// network field have been generated and are ready to be copied
				virtualNetwork, _ := status["virtualNetwork"].(string)
				subnets, _ := status["subnets"].([]interface{})
				securityGroups, _ := status["securityGroups"].([]interface{})
				cluster = cluster.DeepCopy()

				// change fields on status to not be generated
				cluster.Status.EKSStatus.VirtualNetwork = virtualNetwork
				for _, val := range subnets {
					cluster.Status.EKSStatus.Subnets = append(cluster.Status.EKSStatus.Subnets, val.(string))
				}
				for _, val := range securityGroups {
					cluster.Status.EKSStatus.SecurityGroups = append(cluster.Status.EKSStatus.SecurityGroups, val.(string))
				}
				cluster, err = e.ClusterClient.Update(cluster)
				if err != nil {
					return cluster, err
				}
			}
		}

		if cluster.Status.APIEndpoint == "" {
			return e.RecordCAAndAPIEndpoint(cluster)
		}

		if cluster.Status.EKSStatus.PrivateRequiresTunnel == nil && !*cluster.Status.EKSStatus.UpstreamSpec.PublicAccess {
			// Check to see if we can still use the public API endpoint even though
			// the cluster has private-only access
			serviceToken, mustTunnel, err := e.generateSATokenWithPublicAPI(cluster)
			if mustTunnel != nil {
				cluster = cluster.DeepCopy()
				cluster.Status.EKSStatus.PrivateRequiresTunnel = mustTunnel
				cluster.Status.ServiceAccountToken = serviceToken
				return e.ClusterClient.Update(cluster)
			}
			if err != nil {
				return cluster, err
			}
		}

		if cluster.Status.ServiceAccountToken == "" {
			cluster, err = e.generateAndSetServiceAccount(cluster)
			if err != nil {
				var statusErr error
				if strings.Contains(err.Error(), fmt.Sprintf(dialer.WaitForAgentError, cluster.Name)) {
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

		clusterLaunchTemplateID, _ := status["managedLaunchTemplateID"].(string)
		if clusterLaunchTemplateID != "" && cluster.Status.EKSStatus.ManagedLaunchTemplateID != clusterLaunchTemplateID {
			cluster = cluster.DeepCopy()
			cluster.Status.EKSStatus.ManagedLaunchTemplateID = clusterLaunchTemplateID
			cluster, err = e.ClusterClient.Update(cluster)
			if err != nil {
				return cluster, err
			}
		}

		managedLaunchTemplateVersions, _ := status["managedLaunchTemplateVersions"].(map[string]interface{})
		if !reflect.DeepEqual(cluster.Status.EKSStatus.ManagedLaunchTemplateVersions, managedLaunchTemplateVersions) {
			managedLaunchTemplateVersionsToString := make(map[string]string, len(managedLaunchTemplateVersions))
			for key, value := range managedLaunchTemplateVersions {
				managedLaunchTemplateVersionsToString[key] = value.(string)
			}
			cluster.DeepCopy()
			cluster.Status.EKSStatus.ManagedLaunchTemplateVersions = managedLaunchTemplateVersionsToString
			cluster, err = e.ClusterClient.Update(cluster)
			if err != nil {
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
			logrus.Infof("waiting for cluster EKS [%s] to update", cluster.Name)
			return e.SetUnknown(cluster, apimgmtv3.ClusterConditionUpdated, "")
		}
		logrus.Infof("waiting for cluster EKS [%s] update failure to be resolved", cluster.Name)
		return e.SetFalse(cluster, apimgmtv3.ClusterConditionUpdated, failureMessage)
	default:
		if cluster.Spec.EKSConfig.Imported {
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
			if cluster.Spec.EKSConfig.Imported {
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
		logrus.Infof("waiting for cluster EKS [%s] pre-create failure to be resolved", cluster.Name)
		return e.SetFalse(cluster, apimgmtv3.ClusterConditionProvisioned, failureMessage)
	}
}

func (e *eksOperatorController) setInitialUpstreamSpec(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	logrus.Infof("setting initial upstreamSpec on cluster [%s]", cluster.Name)
	cluster = cluster.DeepCopy()
	upstreamSpec, err := clusterupstreamrefresher.BuildEKSUpstreamSpec(e.SecretsCache, cluster)
	if err != nil {
		return cluster, err
	}
	cluster.Status.EKSStatus.UpstreamSpec = upstreamSpec
	return e.ClusterClient.Update(cluster)
}

// updateEKSClusterConfig updates the EKSClusterConfig object's spec with the cluster's EKSConfig if they are not equal..
func (e *eksOperatorController) updateEKSClusterConfig(cluster *mgmtv3.Cluster, eksClusterConfigDynamic *unstructured.Unstructured, spec map[string]interface{}) (*mgmtv3.Cluster, error) {
	list, err := e.DynamicClient.Namespace(namespace.GlobalNamespace).List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return cluster, err
	}
	selector := fields.OneTermEqualSelector("metadata.name", cluster.Name)
	w, err := e.DynamicClient.Namespace(namespace.GlobalNamespace).Watch(context.TODO(), v1.ListOptions{ResourceVersion: list.GetResourceVersion(), FieldSelector: selector.String()})
	if err != nil {
		return cluster, err
	}
	eksClusterConfigDynamic.Object["spec"] = spec
	eksClusterConfigDynamic, err = e.DynamicClient.Namespace(namespace.GlobalNamespace).Update(context.TODO(), eksClusterConfigDynamic, v1.UpdateOptions{})
	if err != nil {
		return cluster, err
	}

	// EKS cluster and node group statuses are not always immediately updated. This cause the EKSConfig to
	// stay in "active" for a few seconds, causing the cluster to go back to "active".
	timeout := time.NewTimer(10 * time.Second)
	for {
		select {
		case event := <-w.ResultChan():
			eksClusterConfigDynamic = event.Object.(*unstructured.Unstructured)
			status, _ := eksClusterConfigDynamic.Object["status"].(map[string]interface{})
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
func (e *eksOperatorController) generateAndSetServiceAccount(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	accessToken, err := e.getAccessToken(cluster)
	if err != nil {
		return cluster, err
	}

	clusterDialer, err := e.ClientDialer.ClusterDialer(cluster.Name)
	if err != nil {
		return cluster, err
	}
	saToken, err := generateSAToken(cluster.Status.APIEndpoint, cluster.Status.CACert, accessToken, clusterDialer)
	if err != nil {
		return cluster, err
	}

	cluster = cluster.DeepCopy()
	cluster.Status.ServiceAccountToken = saToken
	return e.ClusterClient.Update(cluster)
}

// buildEKSCCCreateObject returns an object that can be used with the kubernetes dynamic client to
// create an EKSClusterConfig that matches the spec contained in the cluster's EKSConfig.
func buildEKSCCCreateObject(cluster *mgmtv3.Cluster) (*unstructured.Unstructured, error) {
	eksClusterConfig := eksv1.EKSClusterConfig{
		TypeMeta: v1.TypeMeta{
			Kind:       "EKSClusterConfig",
			APIVersion: eksV1,
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
		Spec: *cluster.Spec.EKSConfig,
	}

	// convert EKS cluster config into unstructured object so it can be used with dynamic client
	eksClusterConfigMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&eksClusterConfig)
	if err != nil {
		return nil, err
	}

	return &unstructured.Unstructured{
		Object: eksClusterConfigMap,
	}, nil
}

// recordAppliedSpec sets the cluster's current spec as its appliedSpec
func (e *eksOperatorController) recordAppliedSpec(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	if reflect.DeepEqual(cluster.Status.AppliedSpec.EKSConfig, cluster.Spec.EKSConfig) {
		return cluster, nil
	}

	cluster = cluster.DeepCopy()
	cluster.Status.AppliedSpec.EKSConfig = cluster.Spec.EKSConfig
	return e.ClusterClient.Update(cluster)
}

// deployEKSOperator looks for the rancher-eks-operator app in the cattle-system namespace, if not found it is deployed.
// If it is found but is outdated, the latest version is installed.
func (e *eksOperatorController) deployEKSOperator() error {
	return e.DeployOperator(eksOperator, eksOperatorTemplate, eksShortName)
}

func (e *eksOperatorController) generateSATokenWithPublicAPI(cluster *mgmtv3.Cluster) (string, *bool, error) {
	var publicAccess *bool
	accessToken, err := e.getAccessToken(cluster)
	if err != nil {
		return "", nil, err
	}

	netDialer := net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	serviceToken, err := generateSAToken(cluster.Status.APIEndpoint, cluster.Status.CACert, accessToken, netDialer.DialContext)
	if err != nil {
		var dnsError *net.DNSError
		if stderrors.As(err, &dnsError) && !dnsError.IsTemporary {
			return "", aws.Bool(true), nil
		}
	} else {
		publicAccess = aws.Bool(false)
	}

	return serviceToken, publicAccess, err
}

func generateSAToken(endpoint, ca, token string, dialer typesDialer.Dialer) (string, error) {
	decodedCA, err := base64.StdEncoding.DecodeString(ca)
	if err != nil {
		return "", err
	}

	restConfig := &rest.Config{
		Host: endpoint,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: decodedCA,
		},
		BearerToken: token,
		Dial:        dialer,
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("error creating clientset: %v", err)
	}

	return util.GenerateServiceAccountToken(clientset)
}

func (e *eksOperatorController) getAccessToken(cluster *mgmtv3.Cluster) (string, error) {
	sess, _, err := controller.StartAWSSessions(e.SecretsCache, *cluster.Spec.EKSConfig)
	if err != nil {
		return "", err
	}
	generator, err := token.NewGenerator(false, false)
	if err != nil {
		return "", err
	}

	awsToken, err := generator.GetWithOptions(&token.GetTokenOptions{
		Session:   sess,
		ClusterID: cluster.Spec.EKSConfig.DisplayName,
	})
	if err != nil {
		return "", err
	}

	return awsToken.Token, nil
}

func notFound(err error) bool {
	if awsErr, ok := err.(awserr.Error); ok {
		return awsErr.Code() == eks.ErrCodeResourceNotFoundException
	}
	return false
}
