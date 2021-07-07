package rbac

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	projectpkg "github.com/rancher/rancher/pkg/project"
	"github.com/rancher/rancher/pkg/settings"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func newProjectLifecycle(r *manager) *pLifecycle {
	return &pLifecycle{m: r}
}

type pLifecycle struct {
	m *manager
}

func (p *pLifecycle) Create(project *v3.Project) (runtime.Object, error) {
	for verb, suffix := range projectNSVerbToSuffix {
		roleName := fmt.Sprintf(projectNSGetClusterRoleNameFmt, project.Name, suffix)
		_, err := p.m.crLister.Get("", roleName)
		if err == nil || !apierrors.IsNotFound(err) {
			continue
		}

		err = p.m.createProjectNSRole(roleName, verb, "")
		if err != nil {
			return project, err
		}

	}

	err := p.ensureNamespacesAssigned(project)
	return project, err
}

func (p *pLifecycle) Updated(project *v3.Project) (runtime.Object, error) {
	return nil, nil
}

func (p *pLifecycle) Remove(project *v3.Project) (runtime.Object, error) {
	for _, suffix := range projectNSVerbToSuffix {
		roleName := fmt.Sprintf(projectNSGetClusterRoleNameFmt, project.Name, suffix)

		err := p.m.workload.RBAC.ClusterRoles("").Delete(roleName, &v1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return project, err
		}
	}

	projectID := project.Namespace + ":" + project.Name
	namespaces, err := p.m.nsIndexer.ByIndex(nsByProjectIndex, projectID)
	if err != nil {
		return project, err
	}

	for _, o := range namespaces {
		namespace, _ := o.(*corev1.Namespace)
		if _, ok := namespace.Annotations["field.cattle.io/creatorId"]; ok {
			err := p.m.workload.Core.Namespaces("").Delete(namespace.Name, &v1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return project, err
			}
		} else {
			namespace = namespace.DeepCopy()
			if namespace.Annotations != nil {
				delete(namespace.Annotations, projectIDAnnotation)
				_, err := p.m.workload.Core.Namespaces("").Update(namespace)
				if err != nil {
					return project, err
				}
			}
		}
	}

	return nil, nil
}

func (p *pLifecycle) ensureNamespacesAssigned(project *v3.Project) error {
	projectName := ""
	if _, ok := project.Labels["authz.management.cattle.io/default-project"]; ok {
		projectName = projectpkg.Default
	} else if _, ok := project.Labels["authz.management.cattle.io/system-project"]; ok {
		projectName = projectpkg.System
	}
	if projectName == "" {
		return nil
	}

	cluster, err := p.m.clusterLister.Get("", p.m.clusterName)
	if err != nil {
		return err
	}
	if cluster == nil {
		return errors.Errorf("couldn't find cluster %v", p.m.clusterName)
	}

	switch projectName {
	case projectpkg.Default:
		if err = p.ensureDefaultNamespaceAssigned(cluster, project); err != nil {
			return err
		}
	case projectpkg.System:
		if err = p.ensureSystemNamespaceAssigned(cluster, project); err != nil {
			return err
		}
	}

	return nil
}

func (p *pLifecycle) ensureDefaultNamespaceAssigned(cluster *v3.Cluster, project *v3.Project) error {
	_, err := v32.ClusterConditionDefaultNamespaceAssigned.DoUntilTrue(cluster.DeepCopy(), func() (runtime.Object, error) {
		return nil, p.assignNamespacesToProject(project, projectpkg.Default)
	})
	return err
}

func (p *pLifecycle) ensureSystemNamespaceAssigned(cluster *v3.Cluster, project *v3.Project) error {
	_, err := v32.ClusterConditionSystemNamespacesAssigned.DoUntilTrue(cluster.DeepCopy(), func() (runtime.Object, error) {
		return nil, p.assignNamespacesToProject(project, projectpkg.System)
	})
	return err
}

func (p *pLifecycle) assignNamespacesToProject(project *v3.Project, projectName string) error {
	initialProjectsToNamespaces, err := getDefaultAndSystemProjectsToNamespaces()
	if err != nil {
		return err
	}
	for _, nsName := range initialProjectsToNamespaces[projectName] {
		ns, err := p.m.nsLister.Get("", nsName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		projectID := ns.Annotations[projectIDAnnotation]
		if projectID != "" {
			return nil
		}

		ns = ns.DeepCopy()
		if ns.Annotations == nil {
			ns.Annotations = map[string]string{}
		}
		ns.Annotations[projectIDAnnotation] = fmt.Sprintf("%v:%v", p.m.clusterName, project.Name)
		if _, err := p.m.workload.Core.Namespaces(p.m.clusterName).Update(ns); err != nil {
			return err
		}
	}
	return nil
}

func getDefaultAndSystemProjectsToNamespaces() (map[string][]string, error) {
	systemNamespacesStr := settings.SystemNamespaces.Get()
	var systemNamespaces []string
	if systemNamespacesStr == "" {
		return nil, fmt.Errorf("failed to load setting %v", settings.SystemNamespaces)
	}

	splitted := strings.Split(systemNamespacesStr, ",")
	for _, s := range splitted {
		systemNamespaces = append(systemNamespaces, strings.TrimSpace(s))
	}

	return map[string][]string{
		projectpkg.Default: {"default"},
		projectpkg.System:  systemNamespaces,
	}, nil
}
