package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"

	"github.com/pkg/errors"
	"github.com/rancher/norman/clientbase"
	"github.com/rancher/norman/controller"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	"github.com/rancher/rancher/pkg/auth/tokens"
	clusterClient "github.com/rancher/rancher/pkg/client/generated/cluster/v3"
	managementClient "github.com/rancher/rancher/pkg/client/generated/management/v3"
	projectClient "github.com/rancher/rancher/pkg/client/generated/project/v3"
	"github.com/rancher/rancher/pkg/controllers/managementlegacy/compose/common"
	"github.com/rancher/rancher/pkg/generated/compose"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/rancher/rancher/pkg/types/config/systemtokens"
	"github.com/rancher/rancher/pkg/user"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	composeTokenPrefix = "compose-token-"
	description        = "token for compose"
	url                = "https://localhost:%v/v3"
)

// Lifecycle for GlobalComposeConfig is a controller which watches composeConfig and execute the yaml config and create a bunch of global resources. There is no sync logic between yaml file and resources, which means config is only executed once. And resource is not deleted even if the compose config is deleted.
type Lifecycle struct {
	TokenClient     v3.TokenInterface
	UserClient      v3.UserInterface
	systemTokens    systemtokens.Interface
	UserManager     user.Manager
	HTTPSPortGetter common.KubeConfigGetter
	ComposeClient   v3.ComposeConfigInterface
}

func Register(ctx context.Context, managementContext *config.ManagementContext, portGetter common.KubeConfigGetter) {
	composeClient := managementContext.Management.ComposeConfigs("")
	tokenClient := managementContext.Management.Tokens("")
	userClient := managementContext.Management.Users("")
	l := Lifecycle{
		systemTokens:    managementContext.SystemTokens,
		HTTPSPortGetter: portGetter,
		UserManager:     managementContext.UserManager,
		TokenClient:     tokenClient,
		UserClient:      userClient,
		ComposeClient:   composeClient,
	}
	composeClient.AddHandler(ctx, "compose-controller", l.sync)
}

func (l Lifecycle) sync(key string, obj *v3.ComposeConfig) (runtime.Object, error) {
	if key == "" || obj == nil {
		return nil, nil
	}
	newObj, err := v32.ComposeConditionExecuted.Once(obj, func() (runtime.Object, error) {
		obj, err := l.Create(obj)
		if err != nil {
			return obj, &controller.ForgetError{
				Err: err,
			}
		}
		return obj, nil
	})

	obj, _ = l.ComposeClient.Update(newObj.(*v3.ComposeConfig))
	return obj, err
}

func (l Lifecycle) Create(obj *v3.ComposeConfig) (*v3.ComposeConfig, error) {
	userID := obj.Annotations["field.cattle.io/creatorId"]
	user, err := l.UserClient.Get(userID, metav1.GetOptions{})
	if err != nil {
		return obj, err
	}
	tokenPrefix := composeTokenPrefix + user.Name
	token, err := l.systemTokens.EnsureSystemToken(tokenPrefix, description, "compose", user.Name, nil, true)
	if err != nil {
		return obj, err
	}
	tokenName, _ := tokens.SplitTokenParts(token)
	defer func() {
		if err := l.systemTokens.DeleteToken(tokenName); err != nil {
			logrus.Errorf("cleanup for compose token [%s] failed, will not retry: %v", tokenName, err)
		}
	}()

	config := &compose.Config{}
	if err := yaml.Unmarshal([]byte(obj.Spec.RancherCompose), config); err != nil {
		return obj, err
	}
	if err := up(token, l.HTTPSPortGetter.GetHTTPSPort(), config); err != nil {
		return obj, err
	}
	v32.ComposeConditionExecuted.True(obj)
	return obj, nil
}

func GetSchemas(token string, port int) (map[string]types.Schema, map[string]types.Schema, map[string]types.Schema, error) {
	cc, err := clusterClient.NewClient(&clientbase.ClientOpts{
		URL:      fmt.Sprintf(url, port) + "/clusters",
		TokenKey: token,
		Insecure: true,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	mc, err := managementClient.NewClient(&clientbase.ClientOpts{
		URL:      fmt.Sprintf(url, port),
		TokenKey: token,
		Insecure: true,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	pc, err := projectClient.NewClient(&clientbase.ClientOpts{
		URL:      fmt.Sprintf(url, port) + "/projects",
		TokenKey: token,
		Insecure: true,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return cc.Types, mc.Types, pc.Types, nil
}

func up(token string, port int, config *compose.Config) error {
	clusterSchemas, managementSchemas, projectSchemas, err := GetSchemas(token, port)
	if err != nil {
		return err
	}

	// referenceMap is a map of schemaType with name -> id value
	referenceMap := map[string]map[string]string{}

	rawData, err := json.Marshal(config)
	if err != nil {
		return err
	}
	rawMap := map[string]interface{}{}
	if err := json.Unmarshal(rawData, &rawMap); err != nil {
		return err
	}
	delete(rawMap, "version")
	allSchemas := getAllSchemas(clusterSchemas, managementSchemas, projectSchemas)
	sortedSchemas := common.SortSchema(allSchemas)

	baseClusterClient, err := clientbase.NewAPIClient(&clientbase.ClientOpts{
		URL:      fmt.Sprintf(url, port) + "/cluster",
		TokenKey: token,
		Insecure: true,
	})
	if err != nil {
		return err
	}
	baseManagementClient, err := clientbase.NewAPIClient(&clientbase.ClientOpts{
		URL:      fmt.Sprintf(url, port),
		TokenKey: token,
		Insecure: true,
	})
	if err != nil {
		return err
	}
	baseProjectClient, err := clientbase.NewAPIClient(&clientbase.ClientOpts{
		URL:      fmt.Sprintf(url, port) + "/project",
		TokenKey: token,
		Insecure: true,
	})
	if err != nil {
		return err
	}
	baseURL := fmt.Sprintf(url, port)
	configManager := configClientManager{
		clusterSchemas:       clusterSchemas,
		managementSchemas:    managementSchemas,
		projectSchemas:       projectSchemas,
		baseClusterClient:    &baseClusterClient,
		baseManagementClient: &baseManagementClient,
		baseProjectClient:    &baseProjectClient,
		baseURL:              baseURL,
	}

	for _, schemaKey := range sortedSchemas {
		key := allSchemas[schemaKey].PluralName
		v, ok := rawMap[key]
		if !ok {
			continue
		}
		value, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		var baseClient *clientbase.APIBaseClient
		for name, data := range value {
			dataMap, ok := data.(map[string]interface{})
			if !ok {
				break
			}
			baseClient, err = configManager.ConfigBaseClient(schemaKey, dataMap, referenceMap, "")
			if err != nil {
				return err
			}
			if err := common.ReplaceGlobalReference(allSchemas[schemaKey], dataMap, referenceMap, &baseManagementClient); err != nil {
				return err
			}
			clusterID := convert.ToString(dataMap["clusterId"])
			baseClient, err = configManager.ConfigBaseClient(schemaKey, dataMap, referenceMap, clusterID)
			if err != nil {
				return err
			}
			dataMap["name"] = name
			respObj := map[string]interface{}{}
			// in here we have to make sure the same name won't be created twice
			created := map[string]string{}
			if err := baseClient.List(schemaKey, &types.ListOpts{}, &respObj); err != nil {
				return err
			}
			if data, ok := respObj["data"]; ok {
				if collections, ok := data.([]interface{}); ok {
					for _, obj := range collections {
						if objMap, ok := obj.(map[string]interface{}); ok {
							createdName := common.GetValue(objMap, "name")
							if createdName != "" {
								created[createdName] = common.GetValue(objMap, "id")
							}
						}
					}
				}
			}

			id := ""
			if v, ok := created[name]; ok {
				id = v
				existing := &types.Resource{}
				if err := baseClient.ByID(schemaKey, id, existing); err != nil {
					return err
				}
				if err := baseClient.Update(schemaKey, existing, dataMap, nil); err != nil {
					return err
				}
			} else {
				if err := baseClient.Create(schemaKey, dataMap, &respObj); err != nil && !strings.Contains(err.Error(), "already exist") {
					return err
				} else if err != nil && strings.Contains(err.Error(), "already exist") {
					break
				}
				v, ok := respObj["id"]
				if !ok {
					return errors.Errorf("id is missing after creating %s obj", schemaKey)
				}
				id = v.(string)
			}
		}
		// fill in reference map name -> id
		if err := common.FillInReferenceMap(baseClient, schemaKey, referenceMap, nil); err != nil {
			return err
		}
	}
	return nil
}

type configClientManager struct {
	clusterSchemas       map[string]types.Schema
	managementSchemas    map[string]types.Schema
	projectSchemas       map[string]types.Schema
	baseClusterClient    *clientbase.APIBaseClient
	baseManagementClient *clientbase.APIBaseClient
	baseProjectClient    *clientbase.APIBaseClient
	baseURL              string
}

// GetBaseClient config a baseClient with a special base url based on schema type
func (c configClientManager) ConfigBaseClient(schemaType string, data map[string]interface{}, referenceMap map[string]map[string]string, clusterID string) (*clientbase.APIBaseClient, error) {
	if _, ok := c.clusterSchemas[schemaType]; ok {
		c.baseClusterClient.Opts.URL = c.baseURL + fmt.Sprintf("/cluster/%s", clusterID)
		return c.baseClusterClient, nil
	}

	if _, ok := c.managementSchemas[schemaType]; ok {
		return c.baseManagementClient, nil
	}

	if _, ok := c.projectSchemas[schemaType]; ok {
		projectName := common.GetValue(data, "projectId")
		if _, ok := referenceMap["project"]; !ok {
			filter := map[string]string{
				"clusterId": clusterID,
			}
			if err := common.FillInReferenceMap(c.baseManagementClient, "project", referenceMap, filter); err != nil {
				return nil, err
			}
		}
		projectID := referenceMap["project"][projectName]
		c.baseProjectClient.Opts.URL = c.baseURL + fmt.Sprintf("/projects/%s", projectID)
		return c.baseProjectClient, nil
	}
	return nil, errors.Errorf("schema type %s not supported", schemaType)
}

func getAllSchemas(clusterSchemas, managementSchemas, projectSchemas map[string]types.Schema) map[string]types.Schema {
	r := map[string]types.Schema{}
	for k, schema := range clusterSchemas {
		if _, ok := schema.ResourceFields["creatorId"]; !ok {
			continue
		}
		r[k] = schema
	}
	for k, schema := range managementSchemas {
		if _, ok := schema.ResourceFields["creatorId"]; !ok {
			continue
		}
		r[k] = schema
	}
	for k, schema := range projectSchemas {
		if _, ok := schema.ResourceFields["creatorId"]; !ok {
			continue
		}
		r[k] = schema
	}
	return r
}
