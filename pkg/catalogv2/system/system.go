package system

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	jsonpatch "github.com/evanphx/json-patch"
	"github.com/rancher/rancher/pkg/api/steve/catalog/types"
	catalog "github.com/rancher/rancher/pkg/apis/catalog.cattle.io/v1"
	"github.com/rancher/rancher/pkg/catalogv2/content"
	"github.com/rancher/rancher/pkg/catalogv2/helmop"
	corev1 "github.com/rancher/rancher/pkg/generated/norman/core/v1"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/action"
	release2 "helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

var (
	installUser = &user.DefaultInfo{
		Name: "helm-installer",
		UID:  "helm-installer",
		Groups: []string{
			"system:masters",
		},
	}
)

type desiredKey struct {
	namespace  string
	name       string
	minVersion string
}

type desired struct {
	key    desiredKey
	values map[string]interface{}
}

type Manager struct {
	ctx              context.Context
	operation        *helmop.Operations
	content          *content.Manager
	restClientGetter genericclioptions.RESTClientGetter
	pods             corecontrollers.PodClient
	desiredCharts    map[desiredKey]map[string]interface{}
	sync             chan desired
	syncLock         sync.Mutex
}

func NewManager(ctx context.Context,
	restClientGetter genericclioptions.RESTClientGetter,
	contentManager *content.Manager,
	ops *helmop.Operations,
	pods corecontrollers.PodClient) (*Manager, error) {

	m := &Manager{
		ctx:              ctx,
		operation:        ops,
		content:          contentManager,
		restClientGetter: restClientGetter,
		pods:             pods,
		sync:             make(chan desired, 10),
		desiredCharts:    map[desiredKey]map[string]interface{}{},
	}

	go m.runSync()
	return m, nil
}

func (m *Manager) runSync() {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.installCharts(m.desiredCharts)
		case desired := <-m.sync:
			v, exists := m.desiredCharts[desired.key]
			m.desiredCharts[desired.key] = desired.values
			// newly requested or changed
			if !exists || !equality.Semantic.DeepEqual(v, desired.values) {
				m.installCharts(map[desiredKey]map[string]interface{}{
					desired.key: desired.values,
				})
			}
		}
	}
}

func (m *Manager) installCharts(charts map[desiredKey]map[string]interface{}) {
	for key, values := range charts {
		for {
			if err := m.install(key.namespace, key.name, key.minVersion, values); err == repo.ErrNoChartName || apierrors.IsNotFound(err) {
				logrus.Errorf("Failed to find system chart %s will try again in 5 seconds: %v", key.name, err)
				time.Sleep(5 * time.Second)
				continue
			} else if err != nil {
				logrus.Errorf("Failed to install system chart %s: %v", key.name, err)
			}
			break
		}
	}
}

func (m *Manager) Ensure(namespace, name, minVersion string, values map[string]interface{}) error {
	go func() {
		m.sync <- desired{
			key: desiredKey{
				namespace:  namespace,
				name:       name,
				minVersion: minVersion,
			},
			values: values,
		}
	}()
	return nil
}

func (m *Manager) install(namespace, name, minVersion string, values map[string]interface{}) error {
	index, err := m.content.Index("", "rancher-charts")
	if err != nil {
		return err
	}

	// get latest, the ~0-a is a weird syntax to match everything including prereleases build
	chart, err := index.Get(name, "~0-a")
	if err != nil {
		return err
	}

	installed, desiredVersion, desiredValue, err := m.isInstalled(namespace, name, chart.Version, minVersion, values)
	if err != nil {
		return err
	} else if installed {
		return nil
	}

	if ok, err := m.isPendingUninstall(namespace, name); err != nil {
		return err
	} else if ok {
		uninstall, err := json.Marshal(types.ChartUninstallAction{
			Timeout: &metav1.Duration{Duration: 5 * time.Minute},
		})
		if err != nil {
			return err
		}

		op, err := m.operation.Uninstall(m.ctx, installUser, namespace, name, bytes.NewBuffer(uninstall))
		if err != nil {
			return err
		}

		if err := m.waitPodDone(op); err != nil {
			return err
		}
	}

	upgrade, err := json.Marshal(types.ChartUpgradeAction{
		Timeout:    &metav1.Duration{Duration: 5 * time.Minute},
		Wait:       true,
		Install:    true,
		MaxHistory: 5,
		Namespace:  namespace,
		Charts: []types.ChartUpgrade{
			{
				ChartName:   name,
				Version:     desiredVersion,
				ReleaseName: name,
				Values:      desiredValue,
				ResetValues: true,
			},
		},
	})
	if err != nil {
		return err
	}

	op, err := m.operation.Upgrade(m.ctx, installUser, "", "rancher-charts", bytes.NewBuffer(upgrade))
	if err != nil {
		return err
	}

	return m.waitPodDone(op)
}

func (m *Manager) waitPodDone(op *catalog.Operation) error {
	pod, err := m.pods.Get(op.Status.PodNamespace, op.Status.PodName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if ok, err := podDone(op.Status.Chart, pod); err != nil {
		return err
	} else if ok {
		return nil
	}

	sec := int64(60)
	resp, err := m.pods.Watch(op.Status.PodNamespace, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + pod.Name,
		ResourceVersion: pod.ResourceVersion,
		TimeoutSeconds:  &sec,
	})
	if err != nil {
		return err
	}
	defer func() {
		go func() {
			for range resp.ResultChan() {
			}
		}()
		resp.Stop()
	}()

	for event := range resp.ResultChan() {
		newPod, ok := event.Object.(*v1.Pod)
		if !ok {
			continue
		}
		if ok, err := podDone(op.Status.Chart, newPod); err != nil {
			return err
		} else if ok {
			return nil
		}
	}

	return fmt.Errorf("pod %s/%s failed, watch closed", pod.Namespace, pod.Name)
}

func podDone(chart string, newPod *corev1.Pod) (bool, error) {
	for _, container := range newPod.Status.ContainerStatuses {
		if container.Name != "helm" {
			continue
		}
		if container.State.Terminated != nil {
			if container.State.Terminated.ExitCode == 0 {
				return true, nil
			}
			return false, fmt.Errorf("failed to install %s, pod %s/%s exited %d", chart,
				newPod.Namespace, newPod.Name, container.State.Terminated.ExitCode)
		}
	}
	return false, nil
}

// isInstalled returns whether the release is installed, if false, it will return the version and values.yaml it should install/upgrade
func (m *Manager) isInstalled(namespace, name, version, minVersion string, desiredValue map[string]interface{}) (bool, string, map[string]interface{}, error) {
	helmcfg := &action.Configuration{}
	if err := helmcfg.Init(m.restClientGetter, namespace, "", logrus.Infof); err != nil {
		return false, "", nil, err
	}

	l := action.NewList(helmcfg)
	l.Filter = "^" + name + "$"

	releases, err := l.Run()
	if err != nil {
		return false, "", nil, err
	}

	desired, err := semver.NewVersion(version)
	if err != nil {
		return false, "", nil, err
	}

	for _, release := range releases {
		if release.Info.Status != release2.StatusDeployed {
			continue
		}
		if desiredValue == nil {
			desiredValue = map[string]interface{}{}
		}
		releaseConfig := release.Config
		if releaseConfig == nil {
			releaseConfig = map[string]interface{}{}
		}

		desiredValuesJSON, err := json.Marshal(desiredValue)
		if err != nil {
			return false, "", nil, err
		}

		actualValueJSON, err := json.Marshal(releaseConfig)
		if err != nil {
			return false, "", nil, err
		}

		patchedJSON, err := jsonpatch.MergePatch(actualValueJSON, desiredValuesJSON)
		if err != nil {
			return false, "", nil, err
		}

		desiredValue = map[string]interface{}{}
		if err := json.Unmarshal(patchedJSON, &desiredValue); err != nil {
			return false, "", nil, err
		}

		ver, err := semver.NewVersion(release.Chart.Metadata.Version)
		if err != nil {
			return false, "", nil, err
		}

		if minVersion != "" {
			min, err := semver.NewVersion(minVersion)
			if err != nil {
				return false, "", nil, err
			}
			if min.LessThan(ver) || min.Equal(ver) {
				// If the current deployed version is greater or equal than the min version but configuration has changed, return false and upgrade with the current version
				if !bytes.Equal(patchedJSON, actualValueJSON) {
					return false, release.Chart.Metadata.Version, desiredValue, nil
				}
				logrus.Debugf("Skipping installing/upgrading desired version %v for release %v, current version %v is greater or equal to minimal required version %v", desired.String(), name, ver.String(), minVersion)
				return true, "", nil, nil
			}
		}

		if (desired.LessThan(ver) || desired.Equal(ver)) && bytes.Equal(patchedJSON, actualValueJSON) {
			return true, "", nil, nil
		}
	}

	return false, version, desiredValue, nil
}

func (m *Manager) isPendingUninstall(namespace, name string) (bool, error) {
	helmcfg := &action.Configuration{}
	if err := helmcfg.Init(m.restClientGetter, namespace, "", logrus.Infof); err != nil {
		return false, err
	}

	l := action.NewList(helmcfg)
	l.Filter = "^" + name + "$"
	l.Pending = true
	l.SetStateMask()

	releases, err := l.Run()
	if err != nil {
		return false, err
	}

	for _, release := range releases {
		if release.Info.Status == release2.StatusPendingInstall {
			return true, nil
		}
	}

	return false, nil
}
