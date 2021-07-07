package rancher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	responsewriter "github.com/rancher/apiserver/pkg/middleware"
	"github.com/rancher/rancher/pkg/api/norman/customization/kontainerdriver"
	"github.com/rancher/rancher/pkg/api/norman/customization/podsecuritypolicytemplate"
	steveapi "github.com/rancher/rancher/pkg/api/steve"
	"github.com/rancher/rancher/pkg/api/steve/aggregation"
	"github.com/rancher/rancher/pkg/api/steve/proxy"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/auth"
	"github.com/rancher/rancher/pkg/auth/audit"
	"github.com/rancher/rancher/pkg/auth/requests"
	"github.com/rancher/rancher/pkg/controllers/dashboard"
	"github.com/rancher/rancher/pkg/controllers/dashboardapi"
	managementauth "github.com/rancher/rancher/pkg/controllers/management/auth"
	crds "github.com/rancher/rancher/pkg/crds/dashboard"
	dashboarddata "github.com/rancher/rancher/pkg/data/dashboard"
	"github.com/rancher/rancher/pkg/features"
	mgmntv3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/multiclustermanager"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/tls"
	"github.com/rancher/rancher/pkg/ui"
	"github.com/rancher/rancher/pkg/websocket"
	"github.com/rancher/rancher/pkg/wrangler"
	aggregation2 "github.com/rancher/steve/pkg/aggregation"
	steveauth "github.com/rancher/steve/pkg/auth"
	steveserver "github.com/rancher/steve/pkg/server"
	"github.com/rancher/wrangler/pkg/k8scheck"
	"github.com/rancher/wrangler/pkg/unstructured"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	k8serror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8dynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const encryptionConfigUpdate = "provisioner.cattle.io/encrypt-migrated"

type Options struct {
	ACMEDomains       cli.StringSlice
	AddLocal          string
	Embedded          bool
	BindHost          string
	HTTPListenPort    int
	HTTPSListenPort   int
	K8sMode           string
	Debug             bool
	Trace             bool
	NoCACerts         bool
	AuditLogPath      string
	AuditLogMaxage    int
	AuditLogMaxsize   int
	AuditLogMaxbackup int
	AuditLevel        int
	Features          string
}

type Rancher struct {
	Auth     steveauth.Middleware
	Handler  http.Handler
	Wrangler *wrangler.Context
	Steve    *steveserver.Server

	auditLog   *audit.LogWriter
	authServer *auth.Server
	opts       *Options
}

func New(ctx context.Context, clientConfg clientcmd.ClientConfig, opts *Options) (*Rancher, error) {
	var (
		authServer *auth.Server
	)

	if opts == nil {
		opts = &Options{}
	}

	restConfig, err := clientConfg.ClientConfig()
	if err != nil {
		return nil, err
	}

	restConfig, err = setupAndValidationRESTConfig(ctx, restConfig)
	if err != nil {
		return nil, err
	}

	// Run the encryption migration before any controllers run otherwise the fields will be dropped
	if err := migrateEncryptionConfig(ctx, restConfig); err != nil {
		return nil, err
	}

	wranglerContext, err := wrangler.NewContext(ctx, clientConfg, restConfig)
	if err != nil {
		return nil, err
	}
	wranglerContext.MultiClusterManager = newMCM(wranglerContext, opts)

	// Initialize Features as early as possible
	if err := crds.CreateFeatureCRD(ctx, restConfig); err != nil {
		return nil, err
	}

	if err := features.MigrateFeatures(wranglerContext.Mgmt.Feature(), wranglerContext.CRD.CustomResourceDefinition()); err != nil {
		return nil, fmt.Errorf("migrating features: %w", err)
	}
	features.InitializeFeatures(wranglerContext.Mgmt.Feature(), opts.Features)

	podsecuritypolicytemplate.RegisterIndexers(wranglerContext)
	kontainerdriver.RegisterIndexers(wranglerContext)
	managementauth.RegisterWranglerIndexers(wranglerContext)

	if err := crds.Create(ctx, restConfig); err != nil {
		return nil, err
	}

	if features.MCM.Enabled() && !features.Fleet.Enabled() {
		logrus.Info("fleet can't be turned off when MCM is enabled. Turning on fleet feature")
		if err := features.SetFeature(wranglerContext.Mgmt.Feature(), features.Fleet.Name(), true); err != nil {
			return nil, err
		}
	}

	if features.Auth.Enabled() {
		authServer, err = auth.NewServer(ctx, restConfig)
		if err != nil {
			return nil, err
		}
	} else {
		authServer, err = auth.NewAlwaysAdmin()
		if err != nil {
			return nil, err
		}
	}

	steve, err := steveserver.New(ctx, restConfig, &steveserver.Options{
		Controllers:     wranglerContext.Controllers,
		AccessSetLookup: wranglerContext.ASL,
		AuthMiddleware:  steveauth.ExistingContext,
		Next:            ui.New(wranglerContext.Mgmt.Preference().Cache(), wranglerContext.Mgmt.ClusterRegistrationToken().Cache()),
	})
	if err != nil {
		return nil, err
	}

	clusterProxy, err := proxy.NewProxyMiddleware(wranglerContext.K8s.AuthorizationV1().SubjectAccessReviews(),
		wranglerContext.TunnelServer.Dialer,
		wranglerContext.Mgmt.Cluster().Cache(),
		localClusterEnabled(opts),
		steve,
	)
	if err != nil {
		return nil, err
	}

	additionalAPIPreMCM := steveapi.AdditionalAPIsPreMCM(wranglerContext)
	additionalAPI, err := steveapi.AdditionalAPIs(ctx, wranglerContext, steve)
	if err != nil {
		return nil, err
	}

	auditLogWriter := audit.NewLogWriter(opts.AuditLogPath, opts.AuditLevel, opts.AuditLogMaxage, opts.AuditLogMaxbackup, opts.AuditLogMaxsize)
	auditFilter, err := audit.NewAuditLogMiddleware(auditLogWriter)
	if err != nil {
		return nil, err
	}
	aggregationMiddleware := aggregation.NewMiddleware(ctx, wranglerContext.Mgmt.APIService(), wranglerContext.TunnelServer)

	return &Rancher{
		Auth: authServer.Authenticator.Chain(
			auditFilter),
		Handler: responsewriter.Chain{
			auth.SetXAPICattleAuthHeader,
			responsewriter.ContentTypeOptions,
			websocket.NewWebsocketHandler,
			proxy.RewriteLocalCluster,
			clusterProxy,
			aggregationMiddleware,
			additionalAPIPreMCM,
			wranglerContext.MultiClusterManager.Middleware,
			authServer.Management,
			additionalAPI,
			requests.NewRequireAuthenticatedFilter("/v1/", "/v1/management.cattle.io.setting"),
		}.Handler(steve),
		Wrangler:   wranglerContext,
		Steve:      steve,
		auditLog:   auditLogWriter,
		authServer: authServer,
		opts:       opts,
	}, nil
}

func (r *Rancher) Start(ctx context.Context) error {
	if err := dashboarddata.EarlyData(ctx, r.Wrangler.K8s); err != nil {
		return err
	}

	if err := dashboardapi.Register(ctx, r.Wrangler); err != nil {
		return err
	}

	if err := steveapi.Setup(ctx, r.Steve, r.Wrangler); err != nil {
		return err
	}

	if features.MCM.Enabled() {
		if err := r.Wrangler.MultiClusterManager.Start(ctx); err != nil {
			return err
		}
	}

	r.Wrangler.OnLeader(func(ctx context.Context) error {
		if err := dashboarddata.Add(ctx, r.Wrangler, localClusterEnabled(r.opts), r.opts.AddLocal == "false", r.opts.Embedded); err != nil {
			return err
		}
		return r.Wrangler.StartWithTransaction(ctx, func(ctx context.Context) error {
			return dashboard.Register(ctx, r.Wrangler)
		})
	})

	if err := r.authServer.Start(ctx, false); err != nil {
		return err
	}

	r.Wrangler.OnLeader(r.authServer.OnLeader)
	r.auditLog.Start(ctx)

	return r.Wrangler.Start(ctx)
}

func (r *Rancher) ListenAndServe(ctx context.Context) error {
	if err := r.Start(ctx); err != nil {
		return err
	}

	r.Wrangler.MultiClusterManager.Wait(ctx)

	r.startAggregation(ctx)
	go r.Steve.StartAggregation(ctx)
	if err := tls.ListenAndServe(ctx, r.Wrangler.RESTConfig,
		r.Auth(r.Handler),
		r.opts.BindHost,
		r.opts.HTTPSListenPort,
		r.opts.HTTPListenPort,
		r.opts.ACMEDomains,
		r.opts.NoCACerts); err != nil {
		return err
	}

	<-ctx.Done()
	return ctx.Err()
}

func (r *Rancher) startAggregation(ctx context.Context) {
	aggregation2.Watch(ctx, r.Wrangler.Core.Secret(), namespace.System, "steve-aggregation", r.Handler)
}

func newMCM(wrangler *wrangler.Context, opts *Options) wrangler.MultiClusterManager {
	return multiclustermanager.NewDeferredServer(wrangler, &multiclustermanager.Options{
		RemoveLocalCluster:  opts.AddLocal == "false",
		LocalClusterEnabled: localClusterEnabled(opts),
		Embedded:            opts.Embedded,
		HTTPSListenPort:     opts.HTTPSListenPort,
		Debug:               opts.Debug,
		Trace:               opts.Trace,
	})
}

func setupAndValidationRESTConfig(ctx context.Context, restConfig *rest.Config) (*rest.Config, error) {
	restConfig = steveserver.RestConfigDefaults(restConfig)
	return restConfig, k8scheck.Wait(ctx, *restConfig)
}

func localClusterEnabled(opts *Options) bool {
	if opts.AddLocal == "true" || opts.AddLocal == "auto" {
		return true
	}
	return false
}

// migrateEncryptionConfig uses the dynamic client to get all clusters and then marshals them through the
// standard go JSON package using the updated backing structs in RKE that include JSON tags. The k8s JSON
// tools are strict with casing so the fields would be dropped before getting saved back in the proper casing
// if any controller touches the cluster first. See https://github.com/rancher/rancher/issues/31385
func migrateEncryptionConfig(ctx context.Context, restConfig *rest.Config) error {
	dynamicClient, err := k8dynamic.NewForConfig(restConfig)
	if err != nil {
		return err
	}
	clusterDynamicClient := dynamicClient.Resource(mgmntv3.ClusterGroupVersionResource)

	clusters, err := clusterDynamicClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		if !k8serror.IsNotFound(err) {
			return err
		}
		// IsNotFound error means the CRD type doesn't exist in the cluster, indicating this is the first Rancher startup
		return nil
	}

	var allErrors error

	for _, c := range clusters.Items {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			rawDynamicCluster, err := clusterDynamicClient.Get(ctx, c.GetName(), metav1.GetOptions{})
			if err != nil {
				return err
			}

			annotations := rawDynamicCluster.GetAnnotations()
			if annotations != nil && annotations[encryptionConfigUpdate] == "true" {
				return nil
			}

			clusterBytes, err := rawDynamicCluster.MarshalJSON()
			if err != nil {
				return errors.Wrap(err, "error trying to Marshal dynamic cluster")
			}

			var cluster *v3.Cluster

			if err := json.Unmarshal(clusterBytes, &cluster); err != nil {
				return errors.Wrap(err, "error trying to Unmarshal dynamicCluster into v3 cluster")
			}

			if cluster.Annotations == nil {
				cluster.Annotations = make(map[string]string)
			}
			cluster.Annotations[encryptionConfigUpdate] = "true"

			u, err := unstructured.ToUnstructured(cluster)
			if err != nil {
				return err
			}

			_, err = clusterDynamicClient.Update(ctx, u, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			allErrors = multierror.Append(err, allErrors)
		}
	}
	return allErrors
}
