package user

import (
	"github.com/rancher/norman/types"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	apitypes "k8s.io/apimachinery/pkg/types"
)

type Manager interface {
	SetPrincipalOnCurrentUser(apiContext *types.APIContext, principal v3.Principal) (*v3.User, error)
	GetUser(apiContext *types.APIContext) string
	EnsureToken(tokenName, description, kind, userName string, ttl *int64, randomize bool) (string, error)
	EnsureClusterToken(clusterName, tokenName, description, kind, userName string, ttl *int64, randomize bool) (string, error)
	DeleteToken(tokenName string) error
	EnsureUser(principalName, displayName string) (*v3.User, error)
	CheckAccess(accessMode string, allowedPrincipalIDs []string, userPrincipalID string, groups []v3.Principal) (bool, error)
	SetPrincipalOnCurrentUserByUserID(userID string, principal v3.Principal) (*v3.User, error)
	CreateNewUserClusterRoleBinding(userName string, userUID apitypes.UID) error
	GetUserByPrincipalID(principalName string) (*v3.User, error)
	GetKubeconfigToken(clusterName, tokenName, description, kind, userName string) (*v3.Token, string, error)
}
