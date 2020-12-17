package clustermanager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rancher/wrangler/pkg/ratelimit"

	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"

	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/norman/httperror"
	"github.com/rancher/norman/types"
	"github.com/rancher/rancher/pkg/clusterrouter"
	clusterController "github.com/rancher/rancher/pkg/controllers/managementuser"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/rbac"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/rancher/rancher/pkg/types/config/dialer"
	"github.com/rancher/rke/pki/cert"
	"github.com/rancher/steve/pkg/accesscontrol"
	rbacv1 "github.com/rancher/wrangler/pkg/generated/controllers/rbac/v1"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authv1 "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Manager struct {
	httpsPort     int
	ScaledContext *config.ScaledContext
	clusterLister v3.ClusterLister
	clusters      v3.ClusterInterface
	controllers   sync.Map
	accessControl types.AccessControl
	rbac          rbacv1.Interface
	dialer        dialer.Factory
	startSem      *semaphore.Weighted
}

type record struct {
	sync.Mutex
	clusterRec    *v3.Cluster
	cluster       *config.UserContext
	accessControl types.AccessControl
	started       bool
	owner         bool
	ctx           context.Context
	cancel        context.CancelFunc
}

func NewManager(httpsPort int, context *config.ScaledContext, rbacControllers rbacv1.Interface, asl accesscontrol.AccessSetLookup) *Manager {
	return &Manager{
		httpsPort:     httpsPort,
		ScaledContext: context,
		accessControl: rbac.NewAccessControlWithASL("", rbacControllers, asl),
		clusterLister: context.Management.Clusters("").Controller().Lister(),
		clusters:      context.Management.Clusters(""),
		startSem:      semaphore.NewWeighted(int64(settings.ClusterControllerStartCount.GetInt())),
	}
}

func (m *Manager) Stop(cluster *v3.Cluster) {
	obj, ok := m.controllers.Load(cluster.UID)
	if !ok {
		return
	}
	logrus.Infof("Stopping cluster agent for %s", obj.(*record).cluster.ClusterName)
	obj.(*record).cancel()
	m.controllers.Delete(cluster.UID)
}

func (m *Manager) Start(ctx context.Context, cluster *v3.Cluster, clusterOwner bool) error {
	if cluster.DeletionTimestamp != nil {
		return nil
	}
	// reload cluster, always use the cached one
	cluster, err := m.clusterLister.Get("", cluster.Name)
	if err != nil {
		return err
	}
	_, err = m.start(ctx, cluster, true, clusterOwner)
	return err
}

func (m *Manager) RESTConfig(cluster *v3.Cluster) (rest.Config, error) {
	obj, ok := m.controllers.Load(cluster.UID)
	if !ok {
		return rest.Config{}, fmt.Errorf("cluster record not found %s %s", cluster.Name, cluster.UID)
	}

	record := obj.(*record)
	return record.cluster.RESTConfig, nil
}

func (m *Manager) markUnavailable(clusterName string) {
	if cluster, err := m.clusters.Get(clusterName, v1.GetOptions{}); err == nil {
		if !v32.ClusterConditionReady.IsFalse(cluster) {
			v32.ClusterConditionReady.False(cluster)
			m.clusters.Update(cluster)
		}
		m.Stop(cluster)
	}
}

func (m *Manager) start(ctx context.Context, cluster *v3.Cluster, controllers, clusterOwner bool) (*record, error) {
	obj, ok := m.controllers.Load(cluster.UID)
	if ok {
		if !m.changed(obj.(*record), cluster, controllers, clusterOwner) {
			return obj.(*record), m.startController(obj.(*record), controllers, clusterOwner)
		}
		m.Stop(obj.(*record).clusterRec)
	}

	clusterRecord, err := m.toRecord(ctx, cluster)
	if err != nil {
		m.markUnavailable(cluster.Name)
		return nil, err
	}
	if clusterRecord == nil {
		return nil, httperror.NewAPIError(httperror.ClusterUnavailable, "cluster not found")
	}

	obj, _ = m.controllers.LoadOrStore(cluster.UID, clusterRecord)
	if err := m.startController(obj.(*record), controllers, clusterOwner); err != nil {
		m.markUnavailable(cluster.Name)
		return nil, err
	}

	return obj.(*record), nil
}

func (m *Manager) startController(r *record, controllers, clusterOwner bool) error {
	if !controllers {
		return nil
	}

	r.Lock()
	defer r.Unlock()
	if !r.started {
		go func() {
			if err := m.doStart(r, clusterOwner); err != nil {
				logrus.Errorf("failed to start cluster controllers %s: %v", r.cluster.ClusterName, err)
				m.markUnavailable(r.clusterRec.Name)
				m.Stop(r.clusterRec)
			}
		}()
		r.started = true
		r.owner = clusterOwner
	}
	return nil
}

func (m *Manager) changed(r *record, cluster *v3.Cluster, controllers, clusterOwner bool) bool {
	existing := r.clusterRec
	if existing.Status.APIEndpoint != cluster.Status.APIEndpoint ||
		existing.Status.ServiceAccountToken != cluster.Status.ServiceAccountToken ||
		existing.Status.CACert != cluster.Status.CACert ||
		existing.Status.AppliedSpec.LocalClusterAuthEndpoint.Enabled != cluster.Status.AppliedSpec.LocalClusterAuthEndpoint.Enabled {
		return true
	}

	if controllers && r.started && clusterOwner != r.owner {
		return true
	}

	return false
}

func (m *Manager) doStart(rec *record, clusterOwner bool) (exit error) {
	defer func() {
		if exit == nil {
			logrus.Infof("Starting cluster agent for %s [owner=%v]", rec.cluster.ClusterName, clusterOwner)
		}
	}()

	for i := 0; ; i++ {
		// Prior to k8s v1.14, we simply did a DiscoveryClient.Version() check to see if the user cluster is alive
		// As of k8s v1.14, kubeapi returns a successful version response even if etcd is not available.
		// To work around this, now we try to get a namespace from the API, even if not found, it means the API is up.
		if _, err := rec.cluster.K8sClient.CoreV1().Namespaces().Get(rec.ctx, "kube-system", v1.GetOptions{}); err != nil && !apierrors.IsNotFound(err) {
			if i == 2 {
				m.markUnavailable(rec.cluster.ClusterName)
			}
			select {
			case <-rec.ctx.Done():
				return rec.ctx.Err()
			case <-time.After(5 * time.Second):
				continue
			}
		}

		break
	}

	if err := m.startSem.Acquire(rec.ctx, 1); err != nil {
		return err
	}
	defer m.startSem.Release(1)

	transaction := controller.NewHandlerTransaction(rec.ctx)
	if clusterOwner {
		if err := clusterController.Register(transaction, rec.cluster, rec.clusterRec, m); err != nil {
			transaction.Rollback()
			return err
		}
	} else {
		if err := clusterController.RegisterFollower(transaction, rec.cluster, m, m); err != nil {
			transaction.Rollback()
			return err
		}
	}

	done := make(chan error, 1)
	go func() {
		defer close(done)

		logrus.Debugf("[clustermanager] creating AccessControl for cluster %v", rec.cluster.ClusterName)
		rec.accessControl = rbac.NewAccessControl(rec.ctx, rec.cluster.ClusterName, rec.cluster.RBACw)

		err := rec.cluster.Start(rec.ctx)
		if err == nil {
			transaction.Commit()
		} else {
			transaction.Rollback()
		}
		done <- err
	}()

	select {
	case <-time.After(10 * time.Minute):
		rec.cancel()
		return fmt.Errorf("timeout syncing controllers")
	case err := <-done:
		return err
	}
}

func ToRESTConfig(cluster *v3.Cluster, context *config.ScaledContext) (*rest.Config, error) {
	if cluster == nil {
		return nil, nil
	}

	if cluster.DeletionTimestamp != nil {
		return nil, nil
	}

	if cluster.Spec.Internal {
		return &context.RESTConfig, nil
	}

	if cluster.Status.APIEndpoint == "" || cluster.Status.CACert == "" || cluster.Status.ServiceAccountToken == "" {
		return nil, nil
	}

	if !v32.ClusterConditionProvisioned.IsTrue(cluster) {
		return nil, nil
	}

	u, err := url.Parse(cluster.Status.APIEndpoint)
	if err != nil {
		return nil, err
	}

	caBytes, err := base64.StdEncoding.DecodeString(cluster.Status.CACert)
	if err != nil {
		return nil, err
	}

	clusterDialer, err := context.Dialer.ClusterDialer(cluster.Name)
	if err != nil {
		return nil, err
	}

	var tlsDialer func(string, string) (net.Conn, error)
	if cluster.Status.Driver == v32.ClusterDriverRKE {
		tlsDialer, err = nameIgnoringTLSDialer(clusterDialer, caBytes)
		if err != nil {
			return nil, err
		}
	}

	// adding suffix to make tlsConfig hashkey unique
	suffix := []byte("\n" + cluster.Name)
	rc := &rest.Config{
		Host:        u.String(),
		BearerToken: cluster.Status.ServiceAccountToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:     append(caBytes, suffix...),
			NextProtos: []string{"http/1.1"},
		},
		Timeout:     45 * time.Second,
		RateLimiter: ratelimit.None,
		UserAgent:   rest.DefaultKubernetesUserAgent() + " cluster " + cluster.Name,
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
			if ht, ok := rt.(*http.Transport); ok {
				if tlsDialer == nil {
					ht.DialContext = clusterDialer
				} else {
					ht.DialContext = nil
					ht.DialTLS = tlsDialer
				}
			}
			return rt
		},
	}

	return rc, nil
}

func nameIgnoringTLSDialer(dialer dialer.Dialer, caBytes []byte) (func(string, string) (net.Conn, error), error) {
	rkeVerify, err := VerifyIgnoreDNSName(caBytes)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		// Use custom TLS validate that validates the cert chain, but not the server.  This should be secure because
		// we use a private per cluster CA always for RKE
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: rkeVerify,
	}

	return func(network, address string) (net.Conn, error) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(15*time.Second))
		defer cancel()
		rawConn, err := dialer(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(rawConn, tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			rawConn.Close()
			return nil, err
		}
		return tlsConn, err
	}, nil
}

func VerifyIgnoreDNSName(caCertsPEM []byte) (func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error, error) {
	rootCAs := x509.NewCertPool()
	if len(caCertsPEM) > 0 {
		caCerts, err := cert.ParseCertsPEM(caCertsPEM)
		if err != nil {
			return nil, err
		}
		for _, cert := range caCerts {
			rootCAs.AddCert(cert)
		}
	}

	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		certs := make([]*x509.Certificate, len(rawCerts))
		for i, asn1Data := range rawCerts {
			cert, err := x509.ParseCertificate(asn1Data)
			if err != nil {
				return fmt.Errorf("failed to parse cert")
			}
			certs[i] = cert
		}

		opts := x509.VerifyOptions{
			Roots:         rootCAs,
			CurrentTime:   time.Now(),
			DNSName:       "",
			Intermediates: x509.NewCertPool(),
		}

		for i, cert := range certs {
			if i == 0 {
				continue
			}
			opts.Intermediates.AddCert(cert)
		}
		_, err := certs[0].Verify(opts)
		return err
	}, nil
}

func (m *Manager) toRecord(ctx context.Context, cluster *v3.Cluster) (*record, error) {
	kubeConfig, err := ToRESTConfig(cluster, m.ScaledContext)
	if kubeConfig == nil || err != nil {
		return nil, err
	}

	clusterContext, err := config.NewUserContext(m.ScaledContext, *kubeConfig, cluster.Name)
	if err != nil {
		return nil, err
	}

	s := &record{
		cluster:    clusterContext,
		clusterRec: cluster,
	}
	s.ctx, s.cancel = context.WithCancel(ctx)

	return s, nil
}

func (m *Manager) AccessControl(apiContext *types.APIContext, storageContext types.StorageContext) (types.AccessControl, error) {
	record, err := m.record(apiContext, storageContext)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return m.accessControl, nil
	}

	if record.accessControl == nil {
		return nil, httperror.NewAPIError(httperror.ClusterUnavailable, "cannot determine access, cluster is unavailable")
	}

	return record.accessControl, nil
}

func (m *Manager) UnversionedClient(apiContext *types.APIContext, storageContext types.StorageContext) (rest.Interface, error) {
	record, err := m.record(apiContext, storageContext)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return m.ScaledContext.UnversionedClient, nil
	}
	return record.cluster.UnversionedClient, nil
}

func (m *Manager) APIExtClient(apiContext *types.APIContext, storageContext types.StorageContext) (clientset.Interface, error) {
	return m.ScaledContext.APIExtClient, nil
}

func (m *Manager) UserContext(clusterName string) (*config.UserContext, error) {
	cluster, err := m.clusterLister.Get("", clusterName)
	if err != nil {
		return nil, err
	}

	record, err := m.start(context.Background(), cluster, false, false)
	if err != nil {
		return nil, httperror.NewAPIError(httperror.ClusterUnavailable, err.Error())
	}

	if record == nil {
		return nil, httperror.NewAPIError(httperror.NotFound, "failed to find cluster")
	}

	return record.cluster, nil
}

func (m *Manager) record(apiContext *types.APIContext, storageContext types.StorageContext) (*record, error) {
	if apiContext == nil {
		return nil, nil
	}
	cluster, err := m.cluster(apiContext, storageContext)
	if err != nil {
		return nil, httperror.NewAPIError(httperror.ClusterUnavailable, err.Error())
	}
	if cluster == nil {
		return nil, nil
	}
	record, err := m.start(context.Background(), cluster, false, false)
	if err != nil {
		return nil, httperror.NewAPIError(httperror.ClusterUnavailable, err.Error())
	}

	return record, nil
}

func (m *Manager) ClusterName(apiContext *types.APIContext) string {
	clusterID := apiContext.SubContext["/v3/schemas/cluster"]
	if clusterID == "" {
		projectID, ok := apiContext.SubContext["/v3/schemas/project"]
		if ok {
			parts := strings.SplitN(projectID, ":", 2)
			if len(parts) == 2 {
				clusterID = parts[0]
			}
		}
	}
	return clusterID
}

func (m *Manager) cluster(apiContext *types.APIContext, context types.StorageContext) (*v3.Cluster, error) {
	switch context {
	case types.DefaultStorageContext:
		return nil, nil
	case config.ManagementStorageContext:
		return nil, nil
	case config.UserStorageContext:
	default:
		return nil, fmt.Errorf("illegal context: %s", context)

	}

	clusterID := m.ClusterName(apiContext)
	if clusterID == "" {
		return nil, nil
	}

	return m.clusterLister.Get("", clusterID)
}

func (m *Manager) KubeConfig(clusterName, token string) *clientcmdapi.Config {
	return &clientcmdapi.Config{
		CurrentContext: "default",
		APIVersion:     "v1",
		Kind:           "Config",
		Clusters: map[string]*clientcmdapi.Cluster{
			"default": {
				Server:                fmt.Sprintf("https://localhost:%d/k8s/clusters/%s", m.httpsPort, clusterName),
				InsecureSkipTLSVerify: true,
			},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"default": {
				AuthInfo: "user",
				Cluster:  "default",
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"user": {
				Token: token,
			},
		},
	}
}

func (m *Manager) GetHTTPSPort() int {
	return m.httpsPort
}

func (m *Manager) SubjectAccessReviewForCluster(req *http.Request) (authv1.SubjectAccessReviewInterface, error) {
	clusterID := clusterrouter.GetClusterID(req)
	userContext, err := m.UserContext(clusterID)
	if err != nil {
		return nil, err
	}
	return userContext.K8sClient.AuthorizationV1().SubjectAccessReviews(), nil
}
