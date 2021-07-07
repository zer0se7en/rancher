package authprovisioningv2

import (
	"fmt"
	"sort"
	"strings"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	apiextcontrollers "github.com/rancher/wrangler/pkg/generated/controllers/apiextensions.k8s.io/v1"
	"github.com/rancher/wrangler/pkg/name"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	clusterIndexed      = "clusterIndexed"
	clusterIndexedLabel = "auth.cattle.io/cluster-indexed"
)

func (h *handler) initializeCRDs(crdClient apiextcontrollers.CustomResourceDefinitionClient) error {
	crds, err := crdClient.List(metav1.ListOptions{
		LabelSelector: clusterIndexedLabel + "=true",
	})
	if err != nil {
		return err
	}

	for _, crd := range crds.Items {
		match := crdToResourceMatch(&crd)
		if match == nil {
			continue
		}
		h.addResources(*match)
	}

	return nil
}

func crdToResourceMatch(crd *apiextv1.CustomResourceDefinition) *resourceMatch {
	if crd.Status.AcceptedNames.Kind == "" || len(crd.Spec.Versions) == 0 {
		return nil
	}

	gvk := schema.GroupVersionKind{
		Group:   crd.Spec.Group,
		Version: crd.Spec.Versions[0].Name,
		Kind:    crd.Status.AcceptedNames.Kind,
	}

	return &resourceMatch{
		GVK:      gvk,
		Resource: crd.Status.AcceptedNames.Plural,
	}
}

func (h *handler) gvkMatcher(gvk schema.GroupVersionKind) bool {
	h.resourcesLock.RLock()
	defer h.resourcesLock.RUnlock()
	_, ok := h.resources[gvk]
	return ok
}

func (h *handler) addResources(resource resourceMatch) {
	h.resourcesLock.Lock()
	defer h.resourcesLock.Unlock()
	h.resources[resource.GVK] = resource

	resources := make([]resourceMatch, 0, len(h.resources))
	for _, v := range h.resources {
		resources = append(resources, v)
	}
	sort.Slice(resources, func(i, j int) bool {
		return resources[i].GVK.String() < resources[j].GVK.String()
	})
	h.resourcesList = resources
}

func (h *handler) OnCRD(key string, crd *apiextv1.CustomResourceDefinition) (*apiextv1.CustomResourceDefinition, error) {
	if crd == nil || crd.Labels[clusterIndexedLabel] != "true" {
		return crd, nil
	}

	resourceMatch := crdToResourceMatch(crd)
	if resourceMatch != nil {
		h.addResources(*resourceMatch)
	}

	return crd, nil
}

func (h *handler) OnClusterObjectChanged(obj runtime.Object) (runtime.Object, error) {
	clusterNames, err := getObjectClusterNames(obj)
	if err != nil {
		return nil, err
	}
	meta, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	for _, clusterName := range clusterNames {
		h.roleTemplateController.Enqueue(fmt.Sprintf("cluster/%s/%s", meta.GetNamespace(), clusterName))
	}
	return obj, nil
}

func (h *handler) OnChange(key string, rt *v3.RoleTemplate) (*v3.RoleTemplate, error) {
	if rt != nil {
		return rt, h.objects(rt, true, nil)
	}

	if strings.HasPrefix(key, "cluster/") {
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			return rt, nil
		}

		cluster, err := h.clusters.Get(parts[1], parts[2])
		if apierror.IsNotFound(err) {
			// ignore not found
			return rt, nil
		} else if err != nil {
			return rt, err
		}

		rts, err := h.roleTemplates.List(labels.Everything())
		if err != nil {
			return rt, err
		}
		for _, rt := range rts {
			if err := h.objects(rt, false, cluster); err != nil {
				return nil, err
			}
		}
	}

	return rt, nil
}

func (h *handler) objects(rt *v3.RoleTemplate, enqueue bool, cluster *v1.Cluster) error {
	var (
		matchResults []match
	)

	if rt.Context != "cluster" {
		return nil
	}

	for _, rule := range rt.Rules {
		if len(rule.NonResourceURLs) > 0 || len(rule.ResourceNames) > 0 {
			continue
		}
		matches, err := h.getMatchingClusterIndexedTypes(rule)
		if err != nil {
			return err
		}
		for _, matched := range matches {
			matchResults = append(matchResults, match{
				Rule: rbacv1.PolicyRule{
					Verbs:     rule.Verbs,
					APIGroups: []string{matched.GVK.Group},
					Resources: []string{matched.Resource},
				},
				Match: matched,
			})
		}
	}

	if len(matchResults) == 0 {
		return nil
	}

	if enqueue {
		crtbs, err := h.clusterRoleTemplateBindings.GetByIndex(crbtByRoleTemplateName, rt.Name)
		if err != nil {
			return err
		}
		for _, crtb := range crtbs {
			h.clusterRoleTemplateBindingController.Enqueue(crtb.Namespace, crtb.Name)
		}
	}

	var clusters []*v1.Cluster
	if cluster == nil {
		var err error
		clusters, err = h.clusters.List("", labels.Everything())
		if err != nil {
			return err
		}
	} else {
		clusters = []*v1.Cluster{cluster}
	}

	for _, cluster := range clusters {
		err := h.createRoleForCluster(rt, matchResults, cluster)
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *handler) getResourceNames(rt *v3.RoleTemplate, resourceMatch resourceMatch, cluster *v1.Cluster) ([]string, error) {
	objs, err := h.dynamic.GetByIndex(resourceMatch.GVK, clusterIndexed, fmt.Sprintf("%s/%s", cluster.Namespace, cluster.Name))
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(objs))
	for _, obj := range objs {
		meta, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		result = append(result, meta.GetName())
	}
	return result, nil
}

func roleTemplateRoleName(rtName, clusterName string) string {
	return name.SafeConcatName("crt", clusterName, rtName)
}

func (h *handler) createRoleForCluster(rt *v3.RoleTemplate, matches []match, cluster *v1.Cluster) error {
	h.roleLocker.Lock(cluster.Namespace + "/" + cluster.Name)
	defer h.roleLocker.Unlock(cluster.Namespace + "/" + cluster.Name)

	role := rbacv1.Role{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleTemplateRoleName(rt.Name, cluster.Name),
			Namespace: cluster.Namespace,
		},
	}

	for _, match := range matches {
		names, err := h.getResourceNames(rt, match.Match, cluster)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			continue
		}
		role.Rules = append(role.Rules, rbacv1.PolicyRule{
			Verbs:         match.Rule.Verbs,
			APIGroups:     match.Rule.APIGroups,
			Resources:     match.Rule.Resources,
			ResourceNames: names,
		})
	}

	return h.apply.
		WithListerNamespace(role.Namespace).
		WithSetID(cluster.Name).
		WithOwner(rt).
		WithSetOwnerReference(true, true).
		ApplyObjects(&role)
}

type match struct {
	Rule  rbacv1.PolicyRule
	Match resourceMatch
}

type resourceMatch struct {
	GVK      schema.GroupVersionKind
	Resource string
}

func (r *resourceMatch) Matches(rule rbacv1.PolicyRule) bool {
	return r.matchesGroup(rule) && r.matchesResource(rule)
}

func (r *resourceMatch) matchesGroup(rule rbacv1.PolicyRule) bool {
	for _, group := range rule.APIGroups {
		if group == "*" || group == r.GVK.Group {
			return true
		}
	}
	return false
}

func (r *resourceMatch) matchesResource(rule rbacv1.PolicyRule) bool {
	for _, resource := range rule.Resources {
		if resource == "*" || resource == r.Resource {
			return true
		}
	}
	return false
}

func (h *handler) candidateTypes() []resourceMatch {
	h.resourcesLock.RLock()
	defer h.resourcesLock.RUnlock()
	return h.resourcesList
}

func (h *handler) getMatchingClusterIndexedTypes(rule rbacv1.PolicyRule) (result []resourceMatch, _ error) {
	for _, candidate := range h.candidateTypes() {
		if candidate.Matches(rule) {
			result = append(result, candidate)
		}
	}
	return
}
