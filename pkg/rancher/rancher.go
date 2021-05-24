package rancher

import (
	"context"
	"fmt"
	"net/http"

	responsewriter "github.com/rancher/apiserver/pkg/middleware"
	"github.com/rancher/rancher/pkg/api/norman/customization/kontainerdriver"
	"github.com/rancher/rancher/pkg/api/norman/customization/podsecuritypolicytemplate"
	steveapi "github.com/rancher/rancher/pkg/api/steve"
	"github.com/rancher/rancher/pkg/api/steve/aggregation"
	"github.com/rancher/rancher/pkg/api/steve/proxy"
	"github.com/rancher/rancher/pkg/auth"
	"github.com/rancher/rancher/pkg/auth/audit"
	"github.com/rancher/rancher/pkg/auth/requests"
	"github.com/rancher/rancher/pkg/controllers/dashboard"
	"github.com/rancher/rancher/pkg/controllers/dashboardapi"
	managementauth "github.com/rancher/rancher/pkg/controllers/management/auth"
	crds "github.com/rancher/rancher/pkg/crds/dashboard"
	dashboarddata "github.com/rancher/rancher/pkg/data/dashboard"
	"github.com/rancher/rancher/pkg/features"
	"github.com/rancher/rancher/pkg/multiclustermanager"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/tls"
	"github.com/rancher/rancher/pkg/ui"
	"github.com/rancher/rancher/pkg/websocket"
	"github.com/rancher/rancher/pkg/wrangler"
	steveauth "github.com/rancher/steve/pkg/auth"
	steveserver "github.com/rancher/steve/pkg/server"
	"github.com/rancher/wrangler/pkg/k8scheck"
	"github.com/urfave/cli"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

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

	wranglerContext, err := wrangler.NewContext(ctx, clientConfg, restConfig)
	if err != nil {
		return nil, err
	}
	wranglerContext.MultiClusterManager = newMCM(wranglerContext, opts)

	// Initialize Features as early as possible
	if err := crds.CreateFeatureCRD(ctx, restConfig); err != nil {
		return nil, err
	}
	features.InitializeFeatures(wranglerContext.Mgmt.Feature(), opts.Features)

	podsecuritypolicytemplate.RegisterIndexers(wranglerContext)
	kontainerdriver.RegisterIndexers(wranglerContext)
	managementauth.RegisterWranglerIndexers(wranglerContext)

	if err := crds.Create(ctx, restConfig); err != nil {
		return nil, err
	}

	if features.MCM.Enabled() && !features.Fleet.Enabled() {
		return nil, fmt.Errorf("multi-cluster-management features requires feature fleet=true")
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
		Controllers:                wranglerContext.Controllers,
		AccessSetLookup:            wranglerContext.ASL,
		AuthMiddleware:             steveauth.ExistingContext,
		AggregationSecretNamespace: namespace.System,
		AggregationSecretName:      "steve-aggregation",
		Next:                       ui.New(wranglerContext.Mgmt.Preference().Cache()),
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
	auditFilter := audit.NewAuditLogMiddleware(auditLogWriter)
	aggregation := aggregation.NewMiddleware(ctx, wranglerContext.Mgmt.APIService(), wranglerContext.TunnelServer)

	return &Rancher{
		Auth: authServer.Authenticator.Chain(
			auditFilter),
		Handler: responsewriter.Chain{
			auth.SetXAPICattleAuthHeader,
			responsewriter.ContentTypeOptions,
			websocket.NewWebsocketHandler,
			proxy.RewriteLocalCluster,
			clusterProxy,
			aggregation,
			additionalAPIPreMCM,
			wranglerContext.MultiClusterManager.Middleware,
			authServer.Management,
			additionalAPI,
			requests.NewRequireAuthenticatedFilter("/v1/", "/v1/management.cattle.io.settings"),
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
