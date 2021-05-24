package clusterconnected

import (
	"context"
	"time"

	"github.com/rancher/rancher/pkg/api/steve/proxy"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	managementcontrollers "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/wrangler"
	"github.com/rancher/remotedialer"
	"github.com/rancher/wrangler/pkg/condition"
	"github.com/rancher/wrangler/pkg/ticker"
	"github.com/sirupsen/logrus"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	Connected = condition.Cond("Connected")
)

func Register(ctx context.Context, wrangler *wrangler.Context) {
	c := checker{
		clusterCache: wrangler.Mgmt.Cluster().Cache(),
		clusters:     wrangler.Mgmt.Cluster(),
		tunnelServer: wrangler.TunnelServer,
	}

	go func() {
		for range ticker.Context(ctx, 15*time.Second) {
			if err := c.check(); err != nil {
				logrus.Errorf("failed to check cluster connectivity: %v", err)
			}
		}
	}()
}

type checker struct {
	clusterCache managementcontrollers.ClusterCache
	clusters     managementcontrollers.ClusterClient
	tunnelServer *remotedialer.Server
}

func (c *checker) check() error {
	clusters, err := c.clusterCache.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, cluster := range clusters {
		if err := c.checkCluster(cluster); err != nil {
			logrus.Errorf("failed to check connectivity of cluster [%s]", cluster.Name)
		}
	}
	return nil
}

func (c *checker) checkCluster(cluster *v3.Cluster) error {
	if cluster.Spec.Internal {
		return nil
	}

	hasSession := c.tunnelServer.HasSession(proxy.Prefix + cluster.Name)
	// The simpler condition of hasSession == Connected.IsTrue(cluster) is not
	// used because it treat a non-existent conditions as False
	if hasSession && Connected.IsTrue(cluster) {
		return nil
	} else if !hasSession && Connected.IsFalse(cluster) {
		return nil
	}

	var (
		err error
	)

	for i := 0; i < 3; i++ {
		cluster = cluster.DeepCopy()
		Connected.SetStatusBool(cluster, hasSession)
		_, err = c.clusters.Update(cluster)
		if apierror.IsConflict(err) {
			cluster, err = c.clusters.Get(cluster.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		return nil
	}

	return err
}
