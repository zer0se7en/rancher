package eks

import (
	"context"
	"encoding/base64"
	stderrors "errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/rancher/eks-operator/controller"
	eksv1 "github.com/rancher/eks-operator/pkg/apis/eks.cattle.io/v1"
	"github.com/rancher/norman/condition"
	apimgmtv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	apiprojv3 "github.com/rancher/rancher/pkg/apis/project.cattle.io/v3"
	utils2 "github.com/rancher/rancher/pkg/app"
	"github.com/rancher/rancher/pkg/catalog/manager"
	"github.com/rancher/rancher/pkg/controllers/management/clusterupstreamrefresher"
	"github.com/rancher/rancher/pkg/controllers/management/rbac"
	"github.com/rancher/rancher/pkg/dialer"
	v3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	corev1 "github.com/rancher/rancher/pkg/generated/norman/core/v1"
	mgmtv3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	projectv3 "github.com/rancher/rancher/pkg/generated/norman/project.cattle.io/v3"
	"github.com/rancher/rancher/pkg/kontainer-engine/drivers/util"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/project"
	"github.com/rancher/rancher/pkg/ref"
	"github.com/rancher/rancher/pkg/systemaccount"
	"github.com/rancher/rancher/pkg/types/config"
	typesDialer "github.com/rancher/rancher/pkg/types/config/dialer"
	"github.com/rancher/rancher/pkg/wrangler"
	wranglerv1 "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
	"sigs.k8s.io/yaml"
)

const (
	systemNS            = "cattle-system"
	eksAPIGroup         = "eks.cattle.io"
	eksV1               = "eks.cattle.io/v1"
	eksOperatorTemplate = "system-library-rancher-eks-operator"
	eksOperator         = "rancher-eks-operator"
	localCluster        = "local"
	enqueueTime         = time.Second * 5
	importedAnno        = "eks.cattle.io/imported"
)

type eksOperatorValues struct {
	HTTPProxy  string `json:"httpProxy,omitempty"`
	HTTPSProxy string `json:"httpsProxy,omitempty"`
	NoProxy    string `json:"noProxy,omitempty"`
}

type eksOperatorController struct {
	clusterEnqueueAfter  func(name string, duration time.Duration)
	secretsCache         wranglerv1.SecretCache
	templateCache        v3.CatalogTemplateCache
	projectCache         v3.ProjectCache
	appLister            projectv3.AppLister
	appClient            projectv3.AppInterface
	nsClient             corev1.NamespaceInterface
	clusterClient        v3.ClusterClient
	catalogManager       manager.CatalogManager
	systemAccountManager *systemaccount.Manager
	dynamicClient        dynamic.NamespaceableResourceInterface
	clientDialer         typesDialer.Factory
}

func Register(ctx context.Context, wContext *wrangler.Context, mgmtCtx *config.ManagementContext) {
	eksClusterConfigResource := schema.GroupVersionResource{
		Group:    eksAPIGroup,
		Version:  "v1",
		Resource: "eksclusterconfigs",
	}

	eksCCDynamicClient := mgmtCtx.DynamicClient.Resource(eksClusterConfigResource)
	e := &eksOperatorController{
		clusterEnqueueAfter:  wContext.Mgmt.Cluster().EnqueueAfter,
		secretsCache:         wContext.Core.Secret().Cache(),
		templateCache:        wContext.Mgmt.CatalogTemplate().Cache(),
		projectCache:         wContext.Mgmt.Project().Cache(),
		appLister:            mgmtCtx.Project.Apps("").Controller().Lister(),
		appClient:            mgmtCtx.Project.Apps(""),
		nsClient:             mgmtCtx.Core.Namespaces(""),
		clusterClient:        wContext.Mgmt.Cluster(),
		catalogManager:       mgmtCtx.CatalogManager,
		systemAccountManager: systemaccount.NewManager(mgmtCtx),
		dynamicClient:        eksCCDynamicClient,
		clientDialer:         mgmtCtx.Dialer,
	}

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
			cluster, conditionErr = e.setFalse(cluster, apimgmtv3.ClusterConditionPending, fmt.Sprintf(failedToDeployEKSOperatorErr, err))
			if conditionErr != nil {
				return cluster, conditionErr
			}
		} else {
			cluster, conditionErr = e.setFalse(cluster, apimgmtv3.ClusterConditionProvisioned, fmt.Sprintf(failedToDeployEKSOperatorErr, err))
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
		cluster, err = e.clusterClient.Update(cluster)
		if err != nil {
			return cluster, err
		}
	}

	// get EKS Cluster Config, if it does not exist, create it
	eksClusterConfigDynamic, err := e.dynamicClient.Namespace(namespace.GlobalNamespace).Get(context.TODO(), cluster.Name, v1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return cluster, err
		}

		cluster, err = e.setUnknown(cluster, apimgmtv3.ClusterConditionWaiting, "Waiting for API to be available")
		if err != nil {
			return cluster, err
		}

		eksClusterConfigDynamic, err = buildEKSCCCreateObject(cluster)
		if err != nil {
			return cluster, err
		}

		eksClusterConfigDynamic, err = e.dynamicClient.Namespace(namespace.GlobalNamespace).Create(context.TODO(), eksClusterConfigDynamic, v1.CreateOptions{})
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
		// set provisioning to unknown
		cluster, err = e.setUnknown(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		if err != nil {
			return cluster, err
		}

		if cluster.Status.EKSStatus.UpstreamSpec == nil {
			cluster, err = e.setInitialUpstreamSpec(cluster)
			if err != nil {
				if !notFound(err) {
					return cluster, err
				}
			}
			return cluster, nil
		}

		e.clusterEnqueueAfter(cluster.Name, enqueueTime)
		if failureMessage == "" {
			logrus.Infof("waiting for cluster EKS [%s] to finish creating", cluster.Name)
			return e.setUnknown(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		}
		logrus.Infof("waiting for cluster EKS [%s] create failure to be resolved", cluster.Name)
		return e.setFalse(cluster, apimgmtv3.ClusterConditionProvisioned, failureMessage)
	case "active":
		if cluster.Spec.EKSConfig.Imported {
			if cluster.Status.EKSStatus.UpstreamSpec == nil {
				// non imported clusters will have already had upstream spec set
				return e.setInitialUpstreamSpec(cluster)
			}

			if apimgmtv3.ClusterConditionPending.IsUnknown(cluster) {
				cluster = cluster.DeepCopy()
				apimgmtv3.ClusterConditionPending.True(cluster)
				cluster, err = e.clusterClient.Update(cluster)
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
			cluster, err = e.setFalse(cluster, apimgmtv3.ClusterConditionWaiting, addNgMessage)
			if err != nil {
				return cluster, err
			}
		} else {
			if apimgmtv3.ClusterConditionWaiting.GetMessage(cluster) == addNgMessage {
				cluster = cluster.DeepCopy()
				apimgmtv3.ClusterConditionWaiting.Message(cluster, "Waiting for API to be available")
				cluster, err = e.clusterClient.Update(cluster)
				if err != nil {
					return cluster, err
				}
			}
		}

		cluster, err = e.setTrue(cluster, apimgmtv3.ClusterConditionProvisioned, "")
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
				cluster, err = e.clusterClient.Update(cluster)
				if err != nil {
					return cluster, err
				}
			}
		}

		if cluster.Status.APIEndpoint == "" {
			return e.recordCAAndAPIEndpoint(cluster)
		}

		if cluster.Status.EKSStatus.PrivateRequiresTunnel == nil && !*cluster.Status.EKSStatus.UpstreamSpec.PublicAccess {
			// Check to see if we can still use the public API endpoint even though
			// the cluster has private-only access
			serviceToken, mustTunnel, err := e.generateSATokenWithPublicAPI(cluster)
			if mustTunnel != nil {
				cluster = cluster.DeepCopy()
				cluster.Status.EKSStatus.PrivateRequiresTunnel = mustTunnel
				cluster.Status.ServiceAccountToken = serviceToken
				return e.clusterClient.Update(cluster)
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
					cluster, statusErr = e.setUnknown(cluster, apimgmtv3.ClusterConditionWaiting, "waiting for cluster agent to be deployed")
					if statusErr == nil {
						e.clusterEnqueueAfter(cluster.Name, enqueueTime)
					}
					return cluster, statusErr
				}
				cluster, statusErr = e.setFalse(cluster, apimgmtv3.ClusterConditionWaiting,
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
			cluster, err = e.clusterClient.Update(cluster)
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
			cluster, err = e.clusterClient.Update(cluster)
			if err != nil {
				return cluster, err
			}
		}

		cluster, err = e.recordAppliedSpec(cluster)
		if err != nil {
			return cluster, err
		}

		return e.setTrue(cluster, apimgmtv3.ClusterConditionUpdated, "")
	case "updating":
		cluster, err = e.setTrue(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		if err != nil {
			return cluster, err
		}

		e.clusterEnqueueAfter(cluster.Name, enqueueTime)
		if failureMessage == "" {
			logrus.Infof("waiting for cluster EKS [%s] to update", cluster.Name)
			return e.setUnknown(cluster, apimgmtv3.ClusterConditionUpdated, "")
		}
		logrus.Infof("waiting for cluster EKS [%s] update failure to be resolved", cluster.Name)
		return e.setFalse(cluster, apimgmtv3.ClusterConditionUpdated, failureMessage)
	default:
		if cluster.Spec.EKSConfig.Imported {
			cluster, err = e.setUnknown(cluster, apimgmtv3.ClusterConditionPending, "")
			if err != nil {
				return cluster, err
			}
			logrus.Infof("waiting for cluster import [%s] to start", cluster.Name)
		} else {
			logrus.Infof("waiting for cluster create [%s] to start", cluster.Name)
		}

		e.clusterEnqueueAfter(cluster.Name, enqueueTime)
		if failureMessage == "" {
			if cluster.Spec.EKSConfig.Imported {
				cluster, err = e.setUnknown(cluster, apimgmtv3.ClusterConditionPending, "")
				if err != nil {
					return cluster, err
				}
				logrus.Infof("waiting for cluster import [%s] to start", cluster.Name)
			} else {
				logrus.Infof("waiting for cluster create [%s] to start", cluster.Name)
			}
			return e.setUnknown(cluster, apimgmtv3.ClusterConditionProvisioned, "")
		}
		logrus.Infof("waiting for cluster EKS [%s] pre-create failure to be resolved", cluster.Name)
		return e.setFalse(cluster, apimgmtv3.ClusterConditionProvisioned, failureMessage)
	}
}

func (e *eksOperatorController) setInitialUpstreamSpec(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	logrus.Infof("setting initial upstreamSpec on cluster [%s]", cluster.Name)
	cluster = cluster.DeepCopy()
	upstreamSpec, err := clusterupstreamrefresher.BuildEKSUpstreamSpec(e.secretsCache, cluster)
	if err != nil {
		return cluster, err
	}
	cluster.Status.EKSStatus.UpstreamSpec = upstreamSpec
	return e.clusterClient.Update(cluster)
}

// updateEKSClusterConfig updates the EKSClusterConfig object's spec with the cluster's EKSConfig if they are not equal..
func (e *eksOperatorController) updateEKSClusterConfig(cluster *mgmtv3.Cluster, eksClusterConfigDynamic *unstructured.Unstructured, spec map[string]interface{}) (*mgmtv3.Cluster, error) {
	list, err := e.dynamicClient.Namespace(namespace.GlobalNamespace).List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return cluster, err
	}
	selector := fields.OneTermEqualSelector("metadata.name", cluster.Name)
	w, err := e.dynamicClient.Namespace(namespace.GlobalNamespace).Watch(context.TODO(), v1.ListOptions{ResourceVersion: list.GetResourceVersion(), FieldSelector: selector.String()})
	if err != nil {
		return cluster, err
	}
	eksClusterConfigDynamic.Object["spec"] = spec
	eksClusterConfigDynamic, err = e.dynamicClient.Namespace(namespace.GlobalNamespace).Update(context.TODO(), eksClusterConfigDynamic, v1.UpdateOptions{})
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
			e.clusterEnqueueAfter(cluster.Name, enqueueTime)
			return e.setUnknown(cluster, apimgmtv3.ClusterConditionUpdated, "")
		case <-timeout.C:
			cluster, err = e.recordAppliedSpec(cluster)
			if err != nil {
				return cluster, err
			}
			return cluster, nil
		}
	}
}

// recordCAAndAPIEndpoint reads the EKSClusterConfig's secret once available. The CA cert and API endpoint are then copied to the cluster status.
func (e *eksOperatorController) recordCAAndAPIEndpoint(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	backoff := wait.Backoff{
		Duration: 2 * time.Second,
		Factor:   2,
		Jitter:   0,
		Steps:    6,
		Cap:      20 * time.Second,
	}

	var caSecret *corev1.Secret
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		var err error
		caSecret, err = e.secretsCache.Get(namespace.GlobalNamespace, cluster.Name)
		if err != nil {
			if !errors.IsNotFound(err) {
				return false, err
			}
			logrus.Infof("waiting for cluster [%s] data needed to generate service account token", cluster.Name)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return cluster, fmt.Errorf("failed waiting for cluster [%s] secret: %s", cluster.Name, err)
	}

	apiEndpoint := string(caSecret.Data["endpoint"])
	caCert := string(caSecret.Data["ca"])
	if cluster.Status.APIEndpoint == apiEndpoint && cluster.Status.CACert == caCert {
		return cluster, nil
	}

	var currentCluster *mgmtv3.Cluster
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentCluster, err = e.clusterClient.Get(cluster.Name, v1.GetOptions{})
		if err != nil {
			return err
		}
		currentCluster.Status.APIEndpoint = apiEndpoint
		currentCluster.Status.CACert = caCert
		currentCluster, err = e.clusterClient.Update(currentCluster)
		return err
	})

	return currentCluster, err
}

// generateAndSetServiceAccount uses the API endpoint and CA cert to generate a service account token. The token is then copied to the cluster status.
func (e *eksOperatorController) generateAndSetServiceAccount(cluster *mgmtv3.Cluster) (*mgmtv3.Cluster, error) {
	accessToken, err := e.getAccessToken(cluster)
	if err != nil {
		return cluster, err
	}

	clusterDialer, err := e.clientDialer.ClusterDialer(cluster.Name)
	if err != nil {
		return cluster, err
	}
	saToken, err := generateSAToken(cluster.Status.APIEndpoint, cluster.Status.CACert, accessToken, clusterDialer)
	if err != nil {
		return cluster, err
	}

	cluster = cluster.DeepCopy()
	cluster.Status.ServiceAccountToken = saToken
	return e.clusterClient.Update(cluster)
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
	return e.clusterClient.Update(cluster)
}

// deployEKSOperator looks for the rancher-eks-operator app in the cattle-system namespace, if not found it is deployed.
// If it is found but is outdated, the latest version is installed.
func (e *eksOperatorController) deployEKSOperator() error {
	template, err := e.templateCache.Get(namespace.GlobalNamespace, eksOperatorTemplate)
	if err != nil {
		return err
	}

	latestTemplateVersion, err := e.catalogManager.LatestAvailableTemplateVersion(template, "local")
	if err != nil {
		return err
	}

	latestVersionID := latestTemplateVersion.ExternalID

	systemProject, err := project.GetSystemProject(localCluster, e.projectCache)
	if err != nil {
		return err
	}

	systemProjectID := ref.Ref(systemProject)
	_, systemProjectName := ref.Parse(systemProjectID)

	valuesYaml, err := generateValuesYaml()
	if err != nil {
		return err
	}

	app, err := e.appLister.Get(systemProjectName, eksOperator)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		logrus.Info("deploying EKS operator into local cluster's system project")
		creator, err := e.systemAccountManager.GetSystemUser(localCluster)
		if err != nil {
			return err
		}

		appProjectName, err := utils2.EnsureAppProjectName(e.nsClient, systemProjectName, localCluster, systemNS, creator.Name)
		if err != nil {
			return err
		}

		desiredApp := &apiprojv3.App{
			ObjectMeta: v1.ObjectMeta{
				Name:      eksOperator,
				Namespace: systemProjectName,
				Annotations: map[string]string{
					rbac.CreatorIDAnn: creator.Name,
				},
			},
			Spec: apiprojv3.AppSpec{
				Description:     "Operator for provisioning EKS clusters",
				ExternalID:      latestVersionID,
				ProjectName:     appProjectName,
				TargetNamespace: systemNS,
			},
		}

		desiredApp.Spec.ValuesYaml = valuesYaml

		// k3s upgrader doesn't exist yet, so it will need to be created
		if _, err = e.appClient.Create(desiredApp); err != nil {
			return err
		}
	} else {
		if app.Spec.ExternalID == latestVersionID && app.Spec.ValuesYaml == valuesYaml {
			// app is up to date, no action needed
			return nil
		}
		logrus.Info("updating EKS operator in local cluster's system project")
		desiredApp := app.DeepCopy()
		desiredApp.Spec.ExternalID = latestVersionID
		desiredApp.Spec.ValuesYaml = valuesYaml
		// new version of k3s upgrade available, update app
		if _, err = e.appClient.Update(desiredApp); err != nil {
			return err
		}
	}

	return nil
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
	sess, _, err := controller.StartAWSSessions(e.secretsCache, *cluster.Spec.EKSConfig)
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

func (e *eksOperatorController) setUnknown(cluster *mgmtv3.Cluster, condition condition.Cond, message string) (*mgmtv3.Cluster, error) {
	if condition.IsUnknown(cluster) && condition.GetMessage(cluster) == message {
		return cluster, nil
	}
	cluster = cluster.DeepCopy()
	condition.Unknown(cluster)
	condition.Message(cluster, message)
	var err error
	cluster, err = e.clusterClient.Update(cluster)
	if err != nil {
		return cluster, fmt.Errorf("failed setting cluster [%s] condition %s unknown with message: %s", cluster.Name, condition, message)
	}
	return cluster, nil
}

func (e *eksOperatorController) setTrue(cluster *mgmtv3.Cluster, condition condition.Cond, message string) (*mgmtv3.Cluster, error) {
	if condition.IsTrue(cluster) && condition.GetMessage(cluster) == message {
		return cluster, nil
	}
	cluster = cluster.DeepCopy()
	condition.True(cluster)
	condition.Message(cluster, message)
	var err error
	clusterName := cluster.Name
	cluster, err = e.clusterClient.Update(cluster)
	if err != nil {
		return cluster, fmt.Errorf("failed setting cluster [%s] condition %s true with message: %s", clusterName, condition, message)
	}
	return cluster, nil
}

func (e *eksOperatorController) setFalse(cluster *mgmtv3.Cluster, condition condition.Cond, message string) (*mgmtv3.Cluster, error) {
	if condition.IsFalse(cluster) && condition.GetMessage(cluster) == message {
		return cluster, nil
	}
	cluster = cluster.DeepCopy()
	condition.False(cluster)
	condition.Message(cluster, message)
	var err error
	cluster, err = e.clusterClient.Update(cluster)
	if err != nil {
		return cluster, fmt.Errorf("failed setting cluster [%s] condition %s false with message: %s", cluster.Name, condition, message)
	}
	return cluster, nil
}

func notFound(err error) bool {
	if awsErr, ok := err.(awserr.Error); ok {
		return awsErr.Code() == eks.ErrCodeResourceNotFoundException
	}
	return false
}

// generateValuesYaml generates a YAML string containing any
// necessary values to override defaults in values.yaml. If
// no defaults need to be overwritten, an empty string will
// be returned.
func generateValuesYaml() (string, error) {
	values := eksOperatorValues{
		HTTPProxy:  os.Getenv("HTTP_PROXY"),
		HTTPSProxy: os.Getenv("HTTPS_PROXY"),
		NoProxy:    os.Getenv("NO_PROXY"),
	}

	valuesYaml, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}

	return string(valuesYaml), nil
}
