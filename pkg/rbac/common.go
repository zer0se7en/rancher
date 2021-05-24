package rbac

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"

	"github.com/pkg/errors"
	"github.com/rancher/norman/types"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/ref"
	"github.com/sirupsen/logrus"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	NamespaceID                       = "namespaceId"
	ProjectID                         = "projectId"
	ClusterID                         = "clusterId"
	GlobalAdmin                       = "admin"
	GlobalRestrictedAdmin             = "restricted-admin"
	ClusterCRDsClusterRole            = "cluster-crd-clusterRole"
	RestrictedAdminClusterRoleBinding = "restricted-admin-rb-cluster"
	ProjectCRDsClusterRole            = "project-crd-clusterRole"
	RestrictedAdminProjectRoleBinding = "restricted-admin-rb-project"
	RestrictedAdminCRForClusters      = "restricted-admin-cr-clusters"
	RestrictedAdminCRBForClusters     = "restricted-admin-crb-clusters"
)

// BuildSubjectFromRTB This function will generate
// PRTB and CRTB to the subject with user, group
// or service account
func BuildSubjectFromRTB(object interface{}) (rbacv1.Subject, error) {
	var userName, groupPrincipalName, groupName, name, kind, sa, namespace string
	if rtb, ok := object.(*v3.ProjectRoleTemplateBinding); ok {
		userName = rtb.UserName
		groupPrincipalName = rtb.GroupPrincipalName
		groupName = rtb.GroupName
		sa = rtb.ServiceAccount
	} else if rtb, ok := object.(*v3.ClusterRoleTemplateBinding); ok {
		userName = rtb.UserName
		groupPrincipalName = rtb.GroupPrincipalName
		groupName = rtb.GroupName
	} else {
		return rbacv1.Subject{}, errors.Errorf("unrecognized roleTemplateBinding type: %v", object)
	}

	if userName != "" {
		name = userName
		kind = "User"
	}

	if groupPrincipalName != "" {
		if name != "" {
			return rbacv1.Subject{}, errors.Errorf("roletemplatebinding has more than one subject fields set: %v", object)
		}
		name = groupPrincipalName
		kind = "Group"
	}

	if groupName != "" {
		if name != "" {
			return rbacv1.Subject{}, errors.Errorf("roletemplatebinding has more than one subject fields set: %v", object)
		}
		name = groupName
		kind = "Group"
	}

	if sa != "" {
		parts := strings.SplitN(sa, ":", 2)
		if len(parts) < 2 {
			return rbacv1.Subject{}, errors.Errorf("service account %s of projectroletemplatebinding is invalid: %v", sa, object)
		}
		namespace = parts[0]
		name = parts[1]
		kind = "ServiceAccount"
	}

	if name == "" {
		return rbacv1.Subject{}, errors.Errorf("roletemplatebinding doesn't have any subject fields set: %v", object)
	}

	return rbacv1.Subject{
		Namespace: namespace,
		Kind:      kind,
		Name:      name,
	}, nil
}

func GrbCRBName(grb *v3.GlobalRoleBinding) string {
	var prefix string
	if grb.GlobalRoleName == GlobalAdmin {
		prefix = "globaladmin-"
	} else {
		prefix = "globalrestrictedadmin-"
	}
	return prefix + GetGRBTargetKey(grb)
}

// GetGRBSubject creates and returns a subject that is
// determined by inspecting the the GRB's target fields
func GetGRBSubject(grb *v3.GlobalRoleBinding) rbacv1.Subject {
	kind := "User"
	name := grb.UserName
	if grb.ClusterName == "" && grb.GroupPrincipalName != "" {
		kind = "Group"
		name = grb.GroupPrincipalName
	}

	return rbacv1.Subject{
		Kind:     kind,
		Name:     name,
		APIGroup: rbacv1.GroupName,
	}
}

// getGRBTargetKey returns a key that uniquely identifies the given GRB's target.
// If a user is being targeted, then the user's name is returned.
// Otherwise, the group principal name is converted to a valid user string and
// is returned.
func GetGRBTargetKey(grb *v3.GlobalRoleBinding) string {
	name := grb.UserName

	if name == "" {
		hasher := sha256.New()
		hasher.Write([]byte(grb.GroupPrincipalName))
		sha := base32.StdEncoding.WithPadding(-1).EncodeToString(hasher.Sum(nil))[:10]
		name = "u-" + strings.ToLower(sha)
	}
	return name
}

// Returns object with available information to check against users permissions, used in combination with CanDo
func ObjFromContext(apiContext *types.APIContext, resource *types.RawResource) map[string]interface{} {
	var obj map[string]interface{}
	if resource != nil && resource.Values["id"] != nil {
		obj = resource.Values
	}
	if obj == nil {
		obj = map[string]interface{}{
			"id": apiContext.ID,
		}
		// collection endpoint without id needs to know which cluster-namespace for rbac check
		if apiContext.Query.Get(ClusterID) != "" {
			obj[NamespaceID] = apiContext.Query.Get(ClusterID)
		}
		if apiContext.Query.Get(ProjectID) != "" {
			_, obj[NamespaceID] = ref.Parse(apiContext.Query.Get(ProjectID))
		}
	}
	return obj
}

func TypeFromContext(apiContext *types.APIContext, resource *types.RawResource) string {
	if resource == nil {
		return apiContext.Type
	}
	return resource.Type
}

func GetRTBLabel(objMeta metav1.ObjectMeta) string {
	return objMeta.Namespace + "_" + objMeta.Name
}

// NameForRoleBinding returns a deterministic name for a RoleBinding with the provided namespace, roleName, and subject
func NameForRoleBinding(namespace string, role rbacv1.RoleRef, subject rbacv1.Subject) string {
	var name strings.Builder
	name.WriteString("rb-")
	name.WriteString(getBindingHash(namespace, role, subject))
	nm := name.String()
	logrus.Debugf("RoleBinding with namespace=%s role.kind=%s role.name=%s subject.kind=%s subject.name=%s has name: %s", namespace, role.Kind, role.Name, subject.Kind, subject.Name, nm)
	return nm
}

// NameForClusterRoleBinding returns a deterministic name for a ClusterRoleBinding with the provided roleName and subject
func NameForClusterRoleBinding(role rbacv1.RoleRef, subject rbacv1.Subject) string {
	var name strings.Builder
	name.WriteString("crb-")
	name.WriteString(getBindingHash("", role, subject))
	nm := name.String()
	logrus.Debugf("ClusterRoleBinding with role.kind=%s role.name=%s subject.kind=%s subject.name=%s has name: %s", role.Kind, role.Name, subject.Kind, subject.Name, nm)
	return nm
}

// getBindingHash returns a hash created from the passed in arguments
// uses base32 encoding for hash, since all characters in encoding scheme are valid in k8s resource names
// probability of collision is: 1/32^10 == 1/(2^5)^10 == 1/2^50 (sufficiently low)
func getBindingHash(namespace string, role rbacv1.RoleRef, subject rbacv1.Subject) string {
	var input strings.Builder
	input.WriteString(namespace)
	input.WriteString(role.Kind)
	input.WriteString(role.Name)
	input.WriteString(subject.Kind)
	input.WriteString(subject.Name)

	hasher := sha256.New()
	hasher.Write([]byte(input.String()))
	digest := base32.StdEncoding.WithPadding(-1).EncodeToString(hasher.Sum(nil))
	return strings.ToLower(digest[:10])
}
