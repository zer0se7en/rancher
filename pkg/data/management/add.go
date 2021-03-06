package management

import (
	"github.com/rancher/rancher/pkg/auth/data"
	"github.com/rancher/rancher/pkg/features"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/rancher/rancher/pkg/wrangler"
)

func Add(wrangler *wrangler.Context, management *config.ManagementContext) error {
	_, err := addRoles(wrangler, management)
	if err != nil {
		return err
	}
	if err := addClusterRoleForNamespacedCRDs(management); err != nil {
		return err
	}

	if err := data.AuthConfigs(management); err != nil {
		return err
	}

	if features.Legacy.Enabled() {
		if err := syncCatalogs(management); err != nil {
			return err
		}
	}

	if err := addDefaultPodSecurityPolicyTemplates(management); err != nil {
		return err
	}

	if err := addKontainerDrivers(management); err != nil {
		return err
	}

	if err := addCattleGlobalNamespaces(management); err != nil {
		return err
	}

	return addMachineDrivers(management)
}
