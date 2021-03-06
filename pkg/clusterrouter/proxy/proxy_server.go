package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/rancher/norman/httperror"
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	dialer2 "github.com/rancher/rancher/pkg/dialer"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/kontainer-engine/drivers/gke"
	"github.com/rancher/rancher/pkg/types/config/dialer"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/httpstream"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/client-go/rest"
)

type RemoteService struct {
	sync.Mutex

	cluster   *v3.Cluster
	transport transportGetter
	url       urlGetter
	auth      authGetter

	factory       dialer.Factory
	clusterLister v3.ClusterLister
	caCert        string
	httpTransport *http.Transport
}

var (
	er = &errorResponder{}
)

type urlGetter func() (url.URL, error)

type authGetter func() (string, error)

type transportGetter func() (http.RoundTripper, error)

type errorResponder struct {
}

func (e *errorResponder) Error(w http.ResponseWriter, req *http.Request, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(err.Error()))
}

func prefix(cluster *v3.Cluster) string {
	return "/k8s/clusters/" + cluster.Name
}

func New(localConfig *rest.Config, cluster *v3.Cluster, clusterLister v3.ClusterLister, factory dialer.Factory) (*RemoteService, error) {
	if cluster.Spec.Internal {
		return NewLocal(localConfig, cluster)
	}
	return NewRemote(cluster, clusterLister, factory)
}

func NewLocal(localConfig *rest.Config, cluster *v3.Cluster) (*RemoteService, error) {
	// the gvk is ignored by us, so just pass in any gvk
	hostURL, _, err := rest.DefaultServerURL(localConfig.Host, localConfig.APIPath, schema.GroupVersion{}, true)
	if err != nil {
		return nil, err
	}

	transport, err := rest.TransportFor(localConfig)
	if err != nil {
		return nil, err
	}

	transportGetter := func() (http.RoundTripper, error) {
		return transport, nil
	}

	rs := &RemoteService{
		cluster: cluster,
		url: func() (url.URL, error) {
			return *hostURL, nil
		},
		transport: transportGetter,
	}
	if localConfig.BearerToken != "" {
		rs.auth = func() (string, error) { return "Bearer " + localConfig.BearerToken, nil }
	} else if localConfig.Password != "" {
		rs.auth = func() (string, error) {
			return "Basic " + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", localConfig.Username, localConfig.Password))), nil
		}
	}

	return rs, nil
}

func NewRemote(cluster *v3.Cluster, clusterLister v3.ClusterLister, factory dialer.Factory) (*RemoteService, error) {
	if !v32.ClusterConditionProvisioned.IsTrue(cluster) {
		return nil, httperror.NewAPIError(httperror.ClusterUnavailable, "cluster not provisioned")
	}

	urlGetter := func() (url.URL, error) {
		newCluster, err := clusterLister.Get("", cluster.Name)
		if err != nil {
			return url.URL{}, err
		}

		u, err := url.Parse(newCluster.Status.APIEndpoint)
		if err != nil {
			return url.URL{}, err
		}
		return *u, nil
	}

	authGetter := func() (string, error) {
		newCluster, err := clusterLister.Get("", cluster.Name)
		if err != nil {
			return "", err
		}

		return "Bearer " + newCluster.Status.ServiceAccountToken, nil
	}

	return &RemoteService{
		cluster:       cluster,
		url:           urlGetter,
		auth:          authGetter,
		clusterLister: clusterLister,
		factory:       factory,
	}, nil
}

func (r *RemoteService) getTransport() (http.RoundTripper, error) {
	if r.transport != nil {
		return r.transport()
	}

	newCluster, err := r.clusterLister.Get("", r.cluster.Name)
	if err != nil {
		return nil, err
	}

	r.Lock()
	defer r.Unlock()

	if r.httpTransport != nil && !r.cacertChanged(newCluster) {
		return r.httpTransport, nil
	}

	transport := &http.Transport{}
	if newCluster.Status.CACert != "" {
		certBytes, err := base64.StdEncoding.DecodeString(newCluster.Status.CACert)
		if err != nil {
			return nil, err
		}
		certs := x509.NewCertPool()
		certs.AppendCertsFromPEM(certBytes)
		transport.TLSClientConfig = &tls.Config{
			RootCAs: certs,
		}
	}

	if r.factory != nil {
		d, err := r.factory.ClusterDialer(newCluster.Name)
		if err != nil {
			return nil, err
		}
		transport.DialContext = d
		if dialer2.IsPublicCloudDriver(newCluster) {
			transport.Proxy = http.ProxyFromEnvironment
		}
	}

	r.caCert = newCluster.Status.CACert
	if r.httpTransport != nil {
		r.httpTransport.CloseIdleConnections()
	}
	r.httpTransport = transport

	return transport, nil
}

func (r *RemoteService) cacertChanged(cluster *v3.Cluster) bool {
	return r.caCert != cluster.Status.CACert
}

func (r *RemoteService) Close() {
	if r.httpTransport != nil {
		r.httpTransport.CloseIdleConnections()
	}
}

func (r *RemoteService) Handler() http.Handler {
	return r
}

func (r *RemoteService) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	u, err := r.url()
	if err != nil {
		er.Error(rw, req, err)
		return
	}

	u.Path = strings.TrimPrefix(req.URL.Path, prefix(r.cluster))
	u.RawQuery = req.URL.RawQuery

	proto := req.Header.Get("X-Forwarded-Proto")
	if proto != "" {
		req.URL.Scheme = proto
	} else if req.TLS == nil {
		req.URL.Scheme = "http"
	} else {
		req.URL.Scheme = "https"
	}

	req.URL.Host = req.Host
	transport, err := r.getTransport()
	if err != nil {
		er.Error(rw, req, err)
		return
	}

	if r.cluster.Status.Driver == "googleKubernetesEngine" && r.cluster.Spec.GenericEngineConfig != nil {
		cred, _ := (*r.cluster.Spec.GenericEngineConfig)["credential"].(string)
		transport, err = gke.Oauth2Transport(context.Background(), transport, cred)
		if err != nil {
			er.Error(rw, req, fmt.Errorf("unable to retrieve token source for GKE oauth2: %v", err))
			return
		}
	} else if r.auth == nil {
		req.Header.Del("Authorization")
	} else {
		token, err := r.auth()
		if err != nil {
			er.Error(rw, req, err)
			return
		}
		req.Header.Set("Authorization", token)
	}

	if httpstream.IsUpgradeRequest(req) {
		upgradeProxy := NewUpgradeProxy(&u, transport)
		upgradeProxy.ServeHTTP(rw, req)
		return
	}

	httpProxy := proxy.NewUpgradeAwareHandler(&u, transport, true, false, er)
	httpProxy.ServeHTTP(rw, req)
}

func (r *RemoteService) Cluster() *v3.Cluster {
	return r.cluster
}

type UpgradeProxy struct {
	Location  *url.URL
	Transport http.RoundTripper
}

func NewUpgradeProxy(location *url.URL, transport http.RoundTripper) *UpgradeProxy {
	return &UpgradeProxy{
		Location:  location,
		Transport: transport,
	}
}

func (p *UpgradeProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	loc := *p.Location
	loc.RawQuery = req.URL.RawQuery

	newReq := req.WithContext(req.Context())
	newReq.Header = utilnet.CloneHeader(req.Header)
	newReq.URL = &loc

	httpProxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: p.Location.Scheme, Host: p.Location.Host})
	httpProxy.Transport = p.Transport
	httpProxy.ServeHTTP(rw, newReq)
}
