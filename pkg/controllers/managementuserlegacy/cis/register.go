package cis

import (
	"context"
	"time"

	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/systemaccount"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Register initializes the controllers and registers
func Register(ctx context.Context, userContext *config.UserContext) {
	clusterName := userContext.ClusterName
	clusterClient := userContext.Management.Management.Clusters(metav1.NamespaceAll)
	var cluster *v3.Cluster
	var err error
	for retry := NumberOfRetriesForClusterGet; retry > 0; retry-- {
		cluster, err = clusterClient.Get(clusterName, metav1.GetOptions{})
		if err == nil {
			break
		}
		time.Sleep(RetryIntervalInMilliseconds * time.Millisecond)
	}
	if err != nil {
		logrus.Errorf("error fetching cluster: %v", err)
		return
	}
	if cluster == nil || cluster.Spec.RancherKubernetesEngineConfig == nil {
		logrus.Infof("Not registering CIS controller for non RKE cluster: %v", clusterName)
		return
	}
	logrus.Infof("Registering CIS controllers for cluster: %v", userContext.ClusterName)

	clusterLister := clusterClient.Controller().Lister()
	projectLister := userContext.Management.Management.Projects(metav1.NamespaceAll).Controller().Lister()

	catalogManager := userContext.Management.CatalogManager

	nsClient := userContext.Core.Namespaces(metav1.NamespaceAll)
	cmClient := userContext.Core.ConfigMaps(v32.DefaultNamespaceForCis)
	cmLister := cmClient.Controller().Lister()
	appClient := userContext.Management.Project.Apps(metav1.NamespaceAll)
	catalogTemplateVersionLister := userContext.Management.Management.CatalogTemplateVersions(metav1.NamespaceAll).Controller().Lister()
	systemAccountManager := systemaccount.NewManager(userContext.Management)

	clusterScanClient := userContext.Management.Management.ClusterScans(clusterName)
	podClient := userContext.Core.Pods(v32.DefaultNamespaceForCis)
	podLister := podClient.Controller().Lister()

	dsClient := userContext.Apps.DaemonSets(v32.DefaultNamespaceForCis)
	dsLister := dsClient.Controller().Lister()

	cisConfig := userContext.Management.Management.CisConfigs(namespace.GlobalNamespace)
	cisConfigLister := cisConfig.Controller().Lister()

	cisBenchmarkVersion := userContext.Management.Management.CisBenchmarkVersions(namespace.GlobalNamespace)
	cisBenchmarkVersionLister := cisBenchmarkVersion.Controller().Lister()

	templateLister := userContext.Management.Management.CatalogTemplates(metav1.NamespaceAll).Controller().Lister()

	// Responsible for syncing the benchmark version info from mgmt ctx
	// to config maps in user cluster
	cisBenchmarkVersionHandler := &cisBenchmarkVersionHandler{
		clusterName:               clusterName,
		projectLister:             projectLister,
		cisBenchmarkVersionLister: cisBenchmarkVersionLister,
		configMapsClient:          cmClient,
		nsClient:                  nsClient,
	}
	cisBenchmarkVersion.AddHandler(ctx, "cisBenchmarkVersionHandler", cisBenchmarkVersionHandler.Sync)

	// Responsible for running the cluster scan, cleaning up etc
	clusterScanHandler := &cisScanHandler{
		clusterClient:                clusterClient,
		appClient:                    appClient,
		catalogTemplateVersionLister: catalogTemplateVersionLister,
		clusterScanClient:            clusterScanClient,
		systemAccountManager:         systemAccountManager,
		clusterNamespace:             userContext.ClusterName,
		nsClient:                     nsClient,
		nodeLister:                   userContext.Core.Nodes("").Controller().Lister(),
		cmLister:                     cmLister,
		clusterLister:                clusterLister,
		catalogManager:               catalogManager,
		projectLister:                projectLister,
		cmClient:                     cmClient,
		cisConfigClient:              cisConfig,
		cisConfigLister:              cisConfigLister,
		cisBenchmarkVersionClient:    cisBenchmarkVersion,
		cisBenchmarkVersionLister:    cisBenchmarkVersionLister,
		podClient:                    podClient,
		podLister:                    podLister,
		dsClient:                     dsClient,
		dsLister:                     dsLister,
		templateLister:               templateLister,
		apiExtensionsClient:          userContext.APIExtClient,
	}
	clusterScanClient.AddClusterScopedLifecycle(ctx, "cisScanHandler", clusterName, clusterScanHandler)

	// Mainly to monitor the completion of runner pod via annotation
	podHandler := &podHandler{
		clusterScanClient: clusterScanClient,
	}
	podClient.AddHandler(ctx, "podHandler", podHandler.Sync)
}
