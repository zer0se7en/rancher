package dashboard

import (
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/wrangler"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func addLocalCluster(embedded bool, wrangler *wrangler.Context) error {
	c := &v3.Cluster{
		ObjectMeta: v1.ObjectMeta{
			Name: "local",
		},
		Spec: v32.ClusterSpec{
			Internal:           true,
			DisplayName:        "local",
			FleetWorkspaceName: "fleet-local",
			ClusterSpecBase: v32.ClusterSpecBase{
				DockerRootDir: settings.InitialDockerRootDir.Get(),
			},
		},
		Status: v32.ClusterStatus{
			Driver: v32.ClusterDriverImported,
			Conditions: []v32.ClusterCondition{
				{
					Type:   "Ready",
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if embedded {
		c.Status.Driver = v32.ClusterDriverLocal
	}

	// Ignore error
	_, err := wrangler.Mgmt.Cluster().Create(c)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func removeLocalCluster(wrangler *wrangler.Context) error {
	// Ignore error
	_ = wrangler.Mgmt.Cluster().Delete("local", &v1.DeleteOptions{})
	return nil
}
