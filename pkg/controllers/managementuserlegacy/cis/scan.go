package cis

import (
	"fmt"
	"time"

	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"

	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func NewCisScan(cluster *v3.Cluster, cisScanConfig *v32.CisScanConfig, nameTmpl string, runType v32.ClusterScanRunType) *v3.ClusterScan {
	controller := true
	name := fmt.Sprintf(nameTmpl, time.Now().UnixNano())
	cs := &v3.ClusterScan{
		ObjectMeta: v1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Name,
			OwnerReferences: []v1.OwnerReference{
				{
					Name:       cluster.Name,
					UID:        cluster.UID,
					APIVersion: cluster.APIVersion,
					Kind:       cluster.Kind,
					Controller: &controller,
				},
			},
		},
		Spec: v32.ClusterScanSpec{
			ScanType:  v32.ClusterScanTypeCis,
			ClusterID: cluster.Name,
			RunType:   runType,
			ScanConfig: v32.ClusterScanConfig{
				CisScanConfig: cisScanConfig,
			},
		},
	}
	v32.ClusterScanConditionCreated.Unknown(cs)
	return cs
}

func NewManualCisScan(cluster *v3.Cluster, cisScanConfig *v32.CisScanConfig) *v3.ClusterScan {
	nameTmpl := ManualScanPrefix + "%v"
	return NewCisScan(cluster, cisScanConfig, nameTmpl, v32.ClusterScanRunTypeManual)
}

func NewScheduledCisScan(cluster *v3.Cluster, cisScanConfig *v32.CisScanConfig) *v3.ClusterScan {
	nameTmpl := ScheduledScanPrefix + "%v"
	return NewCisScan(cluster, cisScanConfig, nameTmpl, v32.ClusterScanRunTypeScheduled)
}

func LaunchScan(
	manual bool,
	cisScanConfig *v32.CisScanConfig,
	cluster *v3.Cluster,
	clusterClient v3.ClusterInterface,
	clusterScanClient v3.ClusterScanInterface,
) (*v3.ClusterScan, error) {
	var err error
	var cisScan *v3.ClusterScan
	if manual {
		cisScan = NewManualCisScan(cluster, cisScanConfig)
	} else {
		cisScan = NewScheduledCisScan(cluster, cisScanConfig)
	}
	cisScan, err = clusterScanClient.Create(cisScan)
	if err != nil {
		logrus.Errorf("LaunchScan: error creating cis scan object: %v", err)
		return nil, fmt.Errorf("failed to create cis scan object")
	}

	updatedCluster := cluster.DeepCopy()
	updatedCluster.Status.CurrentCisRunName = cisScan.Name

	// Can't add either too many retries or longer interval as this an API handler
	for i := 0; i < NumberOfRetriesForClusterUpdate; i++ {
		_, err = clusterClient.Update(updatedCluster)
		if err == nil {
			break
		}
		if !errors.IsConflict(err) {
			return nil, err
		}
		time.Sleep(RetryIntervalInMilliseconds * time.Millisecond)
		cluster, err = clusterClient.Get(cluster.Name, v1.GetOptions{})
		if err != nil {
			logrus.Errorf("error fetching cluster with id %v: %v", cluster.Name, err)
			continue
		}
		updatedCluster = cluster.DeepCopy()
		updatedCluster.Status.CurrentCisRunName = cisScan.Name
	}
	if err != nil {
		logrus.Errorf("LaunchScan: error updating cluster annotation for cluster %v: %v", cluster.Name, err)
		return nil, fmt.Errorf("failed to update cluster annotation for cis scan")
	}
	return cisScan, nil
}
