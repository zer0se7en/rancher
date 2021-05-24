package cluster

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/rancher/norman/api/access"
	"github.com/rancher/norman/types"
	"github.com/rancher/rancher/pkg/auth/tokens"
	mgmtclient "github.com/rancher/rancher/pkg/client/generated/management/v3"
	"github.com/rancher/rancher/pkg/controllers/managementuser/clusterauthtoken/common"
	mgmtv3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/kubeconfig"
	"github.com/rancher/rancher/pkg/settings"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func (a ActionHandler) GenerateKubeconfigActionHandler(actionName string, action *types.Action, apiContext *types.APIContext) error {
	var err error
	var cluster mgmtclient.Cluster
	var nodes []*mgmtv3.Node
	if err = access.ByID(apiContext, apiContext.Version, apiContext.Type, apiContext.ID, &cluster); err != nil {
		return err
	}
	if apiContext.Type == "cluster" {
		nodes, err = a.NodeLister.List(cluster.ID, labels.Everything())
		if err != nil {
			return err
		}
	}

	var (
		cfg      string
		tokenKey string
	)

	endpointEnabled := cluster.LocalClusterAuthEndpoint != nil && cluster.LocalClusterAuthEndpoint.Enabled

	generateToken := strings.EqualFold(settings.KubeconfigGenerateToken.Get(), "true")
	if generateToken {
		// generate token and place it in kubeconfig, token doesn't expire
		if endpointEnabled {
			tokenKey, err = a.ensureClusterToken(cluster.ID, apiContext)
		} else {
			tokenKey, err = a.ensureToken(apiContext)
		}
		if err != nil {
			return err
		}
	}

	host := settings.ServerURL.Get()
	if host == "" {
		host = apiContext.Request.Host
	} else {
		u, err := url.Parse(host)
		if err == nil {
			host = u.Host
		} else {
			host = apiContext.Request.Host
		}
	}

	if endpointEnabled {
		clusterName := apiContext.ID
		clusterClient, err := a.ClusterManager.UserContext(clusterName)
		if err != nil {
			return err
		}

		tokenName, tokenValue := tokens.SplitTokenParts(tokenKey)
		// a lister is not used here because the token was recently created, therefore the lister would likely miss
		token, err := a.TokenClient.Get(tokenName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		clusterAuthToken, err := common.NewClusterAuthToken(token, tokenValue)
		if err != nil {
			return err
		}

		if _, err = clusterClient.Cluster.ClusterAuthTokens("cattle-system").Create(clusterAuthToken); err != nil {
			return err
		}

		cfg, err = kubeconfig.ForClusterTokenBased(&cluster, nodes, apiContext.ID, host, tokenKey)
		if err != nil {
			return err
		}
	} else {
		cfg, err = kubeconfig.ForTokenBased(cluster.Name, apiContext.ID, host, tokenKey)
		if err != nil {
			return err
		}
	}

	data := map[string]interface{}{
		"config": cfg,
		"type":   "generateKubeconfigOutput",
	}
	apiContext.WriteResponse(http.StatusOK, data)
	return nil
}
