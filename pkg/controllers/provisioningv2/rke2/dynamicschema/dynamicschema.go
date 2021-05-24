package dynamicschema

import (
	"context"
	"strings"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	rancherv1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	mgmtcontrollers "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/wrangler"
	"github.com/rancher/wrangler/pkg/crd"
	"github.com/rancher/wrangler/pkg/data/convert"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/schemas"
	"github.com/rancher/wrangler/pkg/schemas/openapi"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	nodeAPIGroup = "rke-node.cattle.io"
)

type handler struct {
	schemaCache       mgmtcontrollers.DynamicSchemaCache
	schemasController mgmtcontrollers.DynamicSchemaController
}

func Register(ctx context.Context, clients *wrangler.Context) {
	h := handler{
		schemaCache:       clients.Mgmt.DynamicSchema().Cache(),
		schemasController: clients.Mgmt.DynamicSchema(),
	}
	mgmtcontrollers.RegisterDynamicSchemaGeneratingHandler(ctx,
		clients.Mgmt.DynamicSchema(),
		clients.Apply.WithCacheTypes(clients.CRD.CustomResourceDefinition()),
		"",
		"dynamic-driver-crd",
		h.OnChange,
		&generic.GeneratingHandlerOptions{
			AllowClusterScoped: true,
		})
}

func getStatusSchema(allSchemas *schemas.Schemas) (*schemas.Schema, error) {
	return allSchemas.Import(rkev1.RKEMachineStatus{})
}

func addConfigSchema(name string, specSchema, statusSchema *schemas.Schema, allSchemas *schemas.Schemas) (string, error) {
	nodeConfigFields := removeKey(specSchema.ResourceFields, "common")
	nodeConfigFields = removeKey(nodeConfigFields, "providerID")
	id := name + "Config"
	return id, allSchemas.AddSchema(schemas.Schema{
		ID:             id,
		ResourceFields: nodeConfigFields,
	})
}

func addMachineSchema(name string, specSchema, statusSchema *schemas.Schema, allSchemas *schemas.Schemas) (string, error) {
	id := name + "Machine"
	return id, allSchemas.AddSchema(schemas.Schema{
		ID: id,
		ResourceFields: map[string]schemas.Field{
			"spec": {
				Type: specSchema.ID,
			},
			"status": {
				Type: statusSchema.ID,
			},
		},
	})
}

func addMachineTemplateSchema(name string, specSchema, statusSchema *schemas.Schema, allSchemas *schemas.Schemas) (string, error) {
	templateTemplateSpecSchemaID := name + "MachineTemplateTemplateSpec"
	err := allSchemas.AddSchema(schemas.Schema{
		ID: templateTemplateSpecSchemaID,
		ResourceFields: map[string]schemas.Field{
			"spec": {
				Type: specSchema.ID,
			},
		},
	})
	if err != nil {
		return "", err
	}

	templateSpecSchemaID := name + "MachineTemplateTemplate"
	err = allSchemas.AddSchema(schemas.Schema{
		ID: templateSpecSchemaID,
		ResourceFields: map[string]schemas.Field{
			"template": {
				Type: templateTemplateSpecSchemaID,
			},
		},
	})
	if err != nil {
		return "", err
	}

	id := name + "MachineTemplate"
	return id, allSchemas.AddSchema(schemas.Schema{
		ID: id,
		ResourceFields: map[string]schemas.Field{
			"spec": {
				Type: templateSpecSchemaID,
			},
			"status": {
				Type: statusSchema.ID,
			},
		},
	})
}

func getSchemas(name string, spec *v3.DynamicSchemaSpec) (string, string, string, *schemas.Schemas, error) {
	allSchemas, err := schemas.NewSchemas()
	if err != nil {
		return "", "", "", nil, err
	}

	specSchema, err := getSpecSchemas(name, allSchemas, spec)
	if err != nil {
		return "", "", "", nil, err
	}

	statusSchema, err := getStatusSchema(allSchemas)
	if err != nil {
		return "", "", "", nil, err
	}

	nodeConfigID, err := addConfigSchema(name, specSchema, statusSchema, allSchemas)
	if err != nil {
		return "", "", "", nil, err
	}

	templateID, err := addMachineTemplateSchema(name, specSchema, statusSchema, allSchemas)
	if err != nil {
		return "", "", "", nil, err
	}

	machineID, err := addMachineSchema(name, specSchema, statusSchema, allSchemas)
	if err != nil {
		return "", "", "", nil, err
	}

	return nodeConfigID, templateID, machineID, allSchemas, nil
}

func removeKey(fields map[string]schemas.Field, key string) map[string]schemas.Field {
	result := map[string]schemas.Field{}
	for k, v := range fields {
		if k != key {
			result[k] = v
		}
	}
	return result
}

func getSpecSchemas(name string, allSchemas *schemas.Schemas, spec *v3.DynamicSchemaSpec) (*schemas.Schema, error) {
	specSchema := schemas.Schema{}
	if err := convert.ToObj(spec, &specSchema); err != nil {
		return nil, err
	}
	specSchema.ID = name + "Spec"

	commonField, err := allSchemas.Import(rkev1.RKECommonNodeConfig{})
	if err != nil {
		return nil, err
	}

	if specSchema.ResourceFields == nil {
		specSchema.ResourceFields = map[string]schemas.Field{}
	}

	specSchema.ResourceFields["common"] = schemas.Field{
		Type: commonField.ID,
	}

	specSchema.ResourceFields["providerID"] = schemas.Field{
		Type: "string",
	}

	for name, field := range specSchema.ResourceFields {
		defMap, ok := field.Default.(map[string]interface{})
		if !ok {
			continue
		}

		// set to nil because if map is len() == 0
		field.Default = nil

		switch field.Type {
		case "string", "password":
			field.Default = defMap["stringValue"]
		case "int":
			field.Default = defMap["intValue"]
		case "boolean":
			field.Default = defMap["boolValue"]
		case "array[string]":
			field.Default = defMap["stringSliceValue"]
		}

		specSchema.ResourceFields[name] = field
	}

	if err := allSchemas.AddSchema(specSchema); err != nil {
		return nil, err
	}

	return allSchemas.Schema(specSchema.ID), nil
}

func (h *handler) OnChange(obj *v3.DynamicSchema, status v3.DynamicSchemaStatus) ([]runtime.Object, v3.DynamicSchemaStatus, error) {
	if obj.Name == "nodetemplateconfig" {
		all, err := h.schemaCache.List(labels.Everything())
		if err != nil {
			return nil, status, err
		}
		for _, schema := range all {
			if schema.Name == "nodetemplateconfig" {
				continue
			}
			h.schemasController.Enqueue(schema.Name)
		}
	}

	name, node, _, err := h.getStyle(obj.Name)
	if err != nil {
		return nil, status, err
	}

	if !node { // only support nodes right now  && !cluster {
		return nil, status, nil
	}

	nodeConfigID, templateID, machineID, schemas, err := getSchemas(name, &obj.Spec)
	if err != nil {
		return nil, status, err
	}

	var result []runtime.Object

	for _, id := range []string{nodeConfigID, templateID, machineID} {
		props, err := openapi.ToOpenAPI(id, schemas)
		if err != nil {
			return nil, status, err
		}
		crd := crd.CRD{
			GVK: schema.GroupVersionKind{
				Group:   nodeAPIGroup,
				Version: rkev1.SchemeGroupVersion.Version,
				Kind:    convert.Capitalize(id),
			},
			Schema: props,
			Labels: map[string]string{
				"cluster.x-k8s.io/v1alpha4": "v1",
			},
			Status: true,
		}

		if nodeConfigID == id {
			crd.GVK.Group = rancherv1.SchemeGroupVersion.Group
		}

		crdObj, err := crd.ToCustomResourceDefinition()
		if err != nil {
			return nil, status, err
		}
		result = append(result, crdObj)
	}

	return result, status, nil
}

func (h *handler) getStyle(name string) (string, bool, bool, error) {
	if !strings.HasSuffix(name, "config") {
		return "", false, false, nil
	}

	for _, typeName := range []string{"nodetemplateconfig", "cluster"} {
		schema, err := h.schemaCache.Get(typeName)
		if apierror.IsNotFound(err) {
			continue
		} else if err != nil {
			return "", false, false, err
		}
		for key := range schema.Spec.ResourceFields {
			if strings.EqualFold(key, name) {
				return strings.TrimSuffix(key, "Config"),
					typeName == "nodetemplateconfig",
					typeName == "cluster",
					nil
			}
		}
	}

	return "", false, false, nil
}
