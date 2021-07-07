package rancher

import (
	"context"
	"io/ioutil"
	"os"
	"sync"

	"github.com/rancher/rancher/pkg/agent/cluster"
	"github.com/rancher/rancher/pkg/features"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/rancher"
	"github.com/rancher/wrangler/pkg/apply"
	corefactory "github.com/rancher/wrangler/pkg/generated/controllers/core"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func Run(ctx context.Context) error {
	if err := setupSteveAggregation(); err != nil {
		return err
	}

	if !features.MCMAgent.Enabled() {
		return nil
	}

	cfg, err := kubeconfig.GetNonInteractiveClientConfig("").ClientConfig()
	if err != nil {
		return err
	}

	core, err := corefactory.NewFactoryFromConfig(cfg)
	if err != nil {
		return err
	}

	h := handler{
		ctx:          ctx,
		serviceCache: core.Core().V1().Service().Cache(),
	}

	core.Core().V1().Service().OnChange(ctx, "rancher-installed", h.OnChange)
	return core.Start(ctx, 1)
}

type handler struct {
	lock            sync.Mutex
	ctx             context.Context
	rancherNotFound *bool
	serviceCache    corecontrollers.ServiceCache
}

func (h *handler) startRancher() {
	clientConfig := kubeconfig.GetNonInteractiveClientConfig("")
	server, err := rancher.New(h.ctx, clientConfig, &rancher.Options{
		HTTPListenPort:  80,
		HTTPSListenPort: 443,
		Features:        os.Getenv("CATTLE_FEATURES"),
		AddLocal:        "true",
	})
	if err != nil {
		logrus.Fatalf("Embedded rancher failed to initialize: %v", err)
	}
	go func() {
		err = server.ListenAndServe(h.ctx)
		logrus.Fatalf("Embedded rancher failed to start: %v", err)
	}()
}

func (h *handler) OnChange(key string, service *corev1.Service) (*corev1.Service, error) {
	h.lock.Lock()
	defer h.lock.Unlock()
	if h.rancherNotFound == nil {
		_, err := h.serviceCache.Get(namespace.System, "rancher")
		if notFound := apierror.IsNotFound(err); notFound {
			h.rancherNotFound = &notFound
			h.startRancher()
		} else if err != nil {
			return nil, err
		} else {
			h.rancherNotFound = &notFound
		}
	}

	if service == nil {
		if key == namespace.System+"/rancher" {
			logrus.Info("Rancher has been uninstalled, restarting")
			os.Exit(0)
		}
	} else if service.Namespace == namespace.System && service.Name == "rancher" && *h.rancherNotFound {
		logrus.Info("Rancher has been installed, restarting")
		os.Exit(0)
	}

	return service, nil
}

func setupSteveAggregation() error {
	c, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	apply, err := apply.NewForConfig(c)
	if err != nil {
		return err
	}

	token, url, err := cluster.TokenAndURL()
	if err != nil {
		return err
	}

	data := map[string][]byte{
		"CATTLE_SERVER":      []byte(url),
		"CATTLE_TOKEN":       []byte(token),
		"CATTLE_CA_CHECKSUM": []byte(cluster.CAChecksum()),
		"url":                []byte(url + "/v3/connect"),
		"token":              []byte("steve-cluster-" + token),
	}

	ca, err := ioutil.ReadFile("/etc/kubernetes/ssl/certs/serverca")
	if os.IsNotExist(err) {
	} else if err != nil {
		return err
	} else {
		data["ca.crt"] = ca
	}

	return apply.
		WithDynamicLookup().
		WithSetID("rancher-steve-aggregation").
		WithListerNamespace(namespace.System).
		ApplyObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace.System,
				Name:      "steve-aggregation",
			},
			Data: data,
		})
}
