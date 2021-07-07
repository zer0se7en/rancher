package node

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rancher/norman/objectclient"
	"github.com/rancher/norman/types/convert"
	"github.com/rancher/norman/types/values"
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	util "github.com/rancher/rancher/pkg/cluster"
	"github.com/rancher/rancher/pkg/clustermanager"
	"github.com/rancher/rancher/pkg/controllers/dashboard/clusterregistrationtoken"
	"github.com/rancher/rancher/pkg/controllers/management/drivers/nodedriver"
	"github.com/rancher/rancher/pkg/encryptedstore"
	corev1 "github.com/rancher/rancher/pkg/generated/norman/core/v1"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/jailer"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/nodeconfig"
	"github.com/rancher/rancher/pkg/ref"
	"github.com/rancher/rancher/pkg/systemaccount"
	"github.com/rancher/rancher/pkg/taints"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/rancher/rancher/pkg/types/config/systemtokens"
	"github.com/rancher/rancher/pkg/user"
	rketypes "github.com/rancher/rke/types"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	v1 "k8s.io/api/core/v1"
	kerror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	defaultEngineInstallURL            = "https://releases.rancher.com/install-docker/17.03.2.sh"
	amazonec2                          = "amazonec2"
	userNodeRemoveCleanupAnnotation    = "cleanup.cattle.io/user-node-remove"
	userNodeRemoveCleanupAnnotationOld = "nodes.management.cattle.io/user-node-remove-cleanup"
	userNodeRemoveFinalizerPrefix      = "clusterscoped.controller.cattle.io/user-node-remove_"
	userNodeRemoveAnnotationPrefix     = "lifecycle.cattle.io/create.user-node-remove_"
)

// SchemaToDriverFields maps Schema field => driver field
// The opposite of this lives in pkg/controllers/management/drivers/nodedriver/machine_driver.go
var SchemaToDriverFields = map[string]map[string]string{
	"aliyunecs":     {"sshKeyContents": "sshKeypath"},
	"amazonec2":     {"sshKeyContents": "sshKeypath", "userdata": "userdata"},
	"azure":         {"customData": "customData"},
	"digitalocean":  {"sshKeyContents": "sshKeyPath", "userdata": "userdata"},
	"exoscale":      {"sshKey": "sshKey", "userdata": "userdata"},
	"openstack":     {"cacert": "cacert", "privateKeyFile": "privateKeyFile", "userDataFile": "userDataFile"},
	"otc":           {"privateKeyFile": "privateKeyFile"},
	"packet":        {"userdata": "userdata"},
	"vmwarevsphere": {"cloudConfig": "cloud-config"},
	"google":        {"authEncodedJson": "authEncodedJson"},
}

func Register(ctx context.Context, management *config.ManagementContext, clusterManager *clustermanager.Manager) {
	secretStore, err := nodeconfig.NewStore(management.Core.Namespaces(""), management.Core)
	if err != nil {
		logrus.Fatal(err)
	}

	nodeClient := management.Management.Nodes("")

	nodeLifecycle := &Lifecycle{
		ctx:                       ctx,
		systemAccountManager:      systemaccount.NewManager(management),
		secretStore:               secretStore,
		nodeClient:                nodeClient,
		nodeTemplateClient:        management.Management.NodeTemplates(""),
		nodePoolLister:            management.Management.NodePools("").Controller().Lister(),
		nodePoolController:        management.Management.NodePools("").Controller(),
		nodeTemplateGenericClient: management.Management.NodeTemplates("").ObjectClient().UnstructuredClient(),
		configMapGetter:           management.K8sClient.CoreV1(),
		clusterLister:             management.Management.Clusters("").Controller().Lister(),
		schemaLister:              management.Management.DynamicSchemas("").Controller().Lister(),
		credLister:                management.Core.Secrets("").Controller().Lister(),
		userManager:               management.UserManager,
		systemTokens:              management.SystemTokens,
		clusterManager:            clusterManager,
		devMode:                   os.Getenv("CATTLE_DEV_MODE") != "",
	}

	nodeClient.AddLifecycle(ctx, "node-controller", nodeLifecycle)
	nodeClient.AddHandler(ctx, "node-controller-sync", nodeLifecycle.sync)
}

type Lifecycle struct {
	ctx                       context.Context
	systemAccountManager      *systemaccount.Manager
	secretStore               *encryptedstore.GenericEncryptedStore
	nodeTemplateGenericClient objectclient.GenericClient
	nodeClient                v3.NodeInterface
	nodeTemplateClient        v3.NodeTemplateInterface
	nodePoolLister            v3.NodePoolLister
	nodePoolController        v3.NodePoolController
	configMapGetter           typedv1.ConfigMapsGetter
	clusterLister             v3.ClusterLister
	schemaLister              v3.DynamicSchemaLister
	credLister                corev1.SecretLister
	userManager               user.Manager
	systemTokens              systemtokens.Interface
	clusterManager            *clustermanager.Manager
	devMode                   bool
}

func (m *Lifecycle) setupCustom(obj *v3.Node) {
	obj.Status.NodeConfig = &rketypes.RKEConfigNode{
		NodeName:         obj.Namespace + ":" + obj.Name,
		HostnameOverride: obj.Spec.RequestedHostname,
		Address:          obj.Spec.CustomConfig.Address,
		InternalAddress:  obj.Spec.CustomConfig.InternalAddress,
		User:             obj.Spec.CustomConfig.User,
		DockerSocket:     obj.Spec.CustomConfig.DockerSocket,
		SSHKey:           obj.Spec.CustomConfig.SSHKey,
		Labels:           obj.Spec.CustomConfig.Label,
		Port:             "22",
		Role:             roles(obj),
		Taints:           taints.GetRKETaintsFromStrings(obj.Spec.CustomConfig.Taints),
	}

	if obj.Status.NodeConfig.User == "" {
		obj.Status.NodeConfig.User = "root"
	}

	obj.Status.InternalNodeStatus.Addresses = []v1.NodeAddress{
		{
			Type:    v1.NodeInternalIP,
			Address: obj.Status.NodeConfig.Address,
		},
	}
}

func isCustom(obj *v3.Node) bool {
	return obj.Spec.CustomConfig != nil && obj.Spec.CustomConfig.Address != ""
}

func (m *Lifecycle) setWaiting(node *v3.Node) {
	v32.NodeConditionRegistered.IsUnknown(node)
	v32.NodeConditionRegistered.Message(node, "waiting to register with Kubernetes")
}

func (m *Lifecycle) Create(obj *v3.Node) (runtime.Object, error) {
	if isCustom(obj) {
		m.setupCustom(obj)
		newObj, err := v32.NodeConditionInitialized.Once(obj, func() (runtime.Object, error) {
			if err := validateCustomHost(obj); err != nil {
				return obj, err
			}
			m.setWaiting(obj)
			return obj, nil
		})
		return newObj.(*v3.Node), err
	}

	if obj.Spec.NodeTemplateName == "" {
		return obj, nil
	}

	newObj, err := v32.NodeConditionInitialized.Once(obj, func() (runtime.Object, error) {
		logrus.Debugf("Called v3.NodeConditionInitialized.Once for [%s] in namespace [%s]", obj.Name, obj.Namespace)
		// Ensure jail is created first, else the function `NewNodeConfig` will create the full jail path (including parent jail directory) and CreateJail will remove the directory as it does not contain a done file
		if !m.devMode {
			err := jailer.CreateJail(obj.Namespace)
			if err != nil {
				return nil, errors.WithMessage(err, "node create jail error")
			}
		}

		nodeConfig, err := nodeconfig.NewNodeConfig(m.secretStore, obj)
		if err != nil {
			return obj, errors.WithMessagef(err, "failed to create node driver config for node [%v]", obj.Name)
		}

		defer nodeConfig.Cleanup()

		err = m.refreshNodeConfig(nodeConfig, obj)
		if err != nil {
			return nil, errors.WithMessagef(err, "unable to create config for node %v", obj.Name)
		}

		template, err := m.getNodeTemplate(obj.Spec.NodeTemplateName)
		if err != nil {
			return obj, err
		}
		obj.Status.NodeTemplateSpec = &template.Spec
		if obj.Spec.RequestedHostname == "" {
			obj.Spec.RequestedHostname = obj.Name
		}

		if obj.Status.NodeTemplateSpec.EngineInstallURL == "" {
			obj.Status.NodeTemplateSpec.EngineInstallURL = defaultEngineInstallURL
		}

		return obj, nil
	})

	return newObj.(*v3.Node), err
}

func (m *Lifecycle) getNodeTemplate(nodeTemplateName string) (*v3.NodeTemplate, error) {
	ns, n := ref.Parse(nodeTemplateName)
	logrus.Debugf("getNodeTemplate parsed [%s] to ns: [%s] and n: [%s]", nodeTemplateName, ns, n)
	return m.nodeTemplateClient.GetNamespaced(ns, n, metav1.GetOptions{})
}

func (m *Lifecycle) getNodePool(nodePoolName string) (*v3.NodePool, error) {
	ns, p := ref.Parse(nodePoolName)
	return m.nodePoolLister.Get(ns, p)
}

func (m *Lifecycle) Remove(obj *v3.Node) (runtime.Object, error) {
	if obj.Status.NodeTemplateSpec == nil {
		if err := m.cleanRKENode(obj); err != nil {
			return obj, err
		}

		return m.deleteV1Node(obj)
	}

	newObj, err := v32.NodeConditionRemoved.DoUntilTrue(obj, func() (runtime.Object, error) {
		found, err := m.isNodeInAppliedSpec(obj)
		if err != nil {
			return obj, err
		}
		if found {
			return obj, errors.New("waiting for node to be removed from cluster")
		}

		if !m.devMode {
			err := jailer.CreateJail(obj.Namespace)
			if err != nil {
				return nil, errors.WithMessage(err, "node remove jail error")
			}
		}

		config, err := nodeconfig.NewNodeConfig(m.secretStore, obj)
		if err != nil {
			return obj, err
		}

		if err := config.Restore(); err != nil {
			return obj, err
		}

		defer config.Remove()

		err = m.refreshNodeConfig(config, obj)
		if err != nil {
			return nil, errors.WithMessagef(err, "unable to refresh config for node %v", obj.Name)
		}

		mExists, err := nodeExists(config.Dir(), obj)
		if err != nil {
			return obj, err
		}

		if mExists {
			logrus.Infof("Removing node %s", obj.Spec.RequestedHostname)
			if err := m.drainNode(obj); err != nil {
				return obj, err
			}
			if err := deleteNode(config.Dir(), obj); err != nil {
				return obj, err
			}
			logrus.Infof("Removing node %s done", obj.Spec.RequestedHostname)
		}

		return obj, nil
	})

	if err != nil {
		return newObj.(*v3.Node), err
	}

	return m.deleteV1Node(newObj.(*v3.Node))
}

func (m *Lifecycle) provision(driverConfig, nodeDir string, obj *v3.Node) (*v3.Node, error) {
	configRawMap := map[string]interface{}{}
	if err := json.Unmarshal([]byte(driverConfig), &configRawMap); err != nil {
		return obj, errors.Wrap(err, "failed to unmarshal node config")
	}

	// Since we know this will take a long time persist so user sees status
	obj, err := m.nodeClient.Update(obj)
	if err != nil {
		return obj, err
	}

	err = aliasToPath(obj.Status.NodeTemplateSpec.Driver, configRawMap, obj.Namespace)
	if err != nil {
		return obj, err
	}

	createCommandsArgs := buildCreateCommand(obj, configRawMap)
	cmd, err := buildCommand(nodeDir, obj, createCommandsArgs)
	if err != nil {
		return obj, err
	}

	logrus.Infof("Provisioning node %s", obj.Spec.RequestedHostname)

	stdoutReader, stderrReader, err := startReturnOutput(cmd)
	if err != nil {
		return obj, err
	}
	defer stdoutReader.Close()
	defer stderrReader.Close()
	defer cmd.Wait()

	obj, err = m.reportStatus(stdoutReader, stderrReader, obj)
	if err != nil {
		return obj, err
	}

	if err := cmd.Wait(); err != nil {
		return obj, err
	}

	if err := m.deployAgent(nodeDir, obj); err != nil {
		return obj, err
	}

	logrus.Infof("Provisioning node %s done", obj.Spec.RequestedHostname)
	return obj, nil
}

func aliasToPath(driver string, config map[string]interface{}, ns string) error {
	devMode := os.Getenv("CATTLE_DEV_MODE") != ""
	baseDir := path.Join("/opt/jail", ns)
	if devMode {
		baseDir = os.TempDir()
	}
	// Check if the required driver has aliased fields
	if fields, ok := SchemaToDriverFields[driver]; ok {
		hasher := sha256.New()
		for schemaField, driverField := range fields {
			if fileRaw, ok := config[schemaField]; ok {
				fileContents := fileRaw.(string)
				// Delete our aliased fields
				delete(config, schemaField)
				if fileContents == "" {
					continue
				}

				fileName := driverField
				if ok := nodedriver.SSHKeyFields[schemaField]; ok {
					fileName = "id_rsa"
				}

				// The ending newline gets stripped, add em back
				if !strings.HasSuffix(fileContents, "\n") {
					fileContents = fileContents + "\n"
				}

				hasher.Reset()
				hasher.Write([]byte(fileContents))
				sha := base32.StdEncoding.WithPadding(-1).EncodeToString(hasher.Sum(nil))[:10]

				fileDir := path.Join(baseDir, sha)

				// Delete the fileDir path if it's not a directory
				if info, err := os.Stat(fileDir); err == nil && !info.IsDir() {
					if err := os.Remove(fileDir); err != nil {
						return err
					}
				}

				err := os.MkdirAll(fileDir, 0755)
				if err != nil {
					return err
				}
				fullPath := path.Join(fileDir, fileName)
				err = ioutil.WriteFile(fullPath, []byte(fileContents), 0600)
				if err != nil {
					return err
				}
				// Add the field and path
				if devMode {
					config[driverField] = fullPath
				} else {
					config[driverField] = path.Join("/", sha, fileName)
				}
			}
		}
	}
	return nil
}

func (m *Lifecycle) deployAgent(nodeDir string, obj *v3.Node) error {
	token, err := m.systemAccountManager.GetOrCreateSystemClusterToken(obj.Namespace)
	if err != nil {
		return err
	}

	cluster, err := m.clusterLister.Get("", obj.Namespace)
	if err != nil {
		return err
	}

	err = m.authenticateRegistry(nodeDir, obj, cluster)
	if err != nil {
		return err
	}

	drun, err := clusterregistrationtoken.NodeCommand(token, cluster)
	if err != nil {
		return err
	}
	args := buildAgentCommand(obj, drun)
	cmd, err := buildCommand(nodeDir, obj, args)
	if err != nil {
		return err
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrap(err, string(output))
	}

	return nil
}

// authenticateRegistry authenticates the machine to a private registry if one is defined on the cluster
// this enables the agent image to be pulled from the private registry
func (m *Lifecycle) authenticateRegistry(nodeDir string, node *v3.Node, cluster *v3.Cluster) error {
	reg := util.GetPrivateRepo(cluster)
	// if there is no private registry defined or there is a registry without credentials, return since auth is not needed
	if reg == nil || reg.User == "" || reg.Password == "" {
		return nil
	}

	logrus.Infof("[node-controller-rancher-machine] private registry detected, authenticating %s to %s", node.Spec.RequestedHostname, reg.URL)

	login := clusterregistrationtoken.LoginCommand(*reg)
	args := buildLoginCommand(node, login)
	cmd, err := buildCommand(nodeDir, node, args)
	if err != nil {
		return err
	}

	logrus.Tracef("[node-controller-rancher-machine] login command: %s", cmd.String())

	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrap(err, string(output))
	}

	return nil
}

func (m *Lifecycle) ready(obj *v3.Node) (*v3.Node, error) {
	config, err := nodeconfig.NewNodeConfig(m.secretStore, obj)
	if err != nil {
		return obj, err
	}
	defer config.Cleanup()

	if err := config.Restore(); err != nil {
		return obj, err
	}

	err = m.refreshNodeConfig(config, obj)
	if err != nil {
		return nil, errors.WithMessagef(err, "unable to refresh config for node %v", obj.Name)
	}

	driverConfig, err := config.DriverConfig()
	if err != nil {
		return nil, err
	}

	// Provision in the background so we can poll and save the config
	done := make(chan error)
	go func() {
		newObj, err := m.provision(driverConfig, config.Dir(), obj)
		obj = newObj
		done <- err
	}()

	// Poll and save config
outer:
	for {
		select {
		case err = <-done:
			break outer
		case <-time.After(5 * time.Second):
			config.Save()
		}
	}

	newObj, saveError := v32.NodeConditionConfigSaved.Once(obj, func() (runtime.Object, error) {
		return m.saveConfig(config, config.FullDir(), obj)
	})
	obj = newObj.(*v3.Node)
	if err == nil {
		return obj, saveError
	}
	return obj, err
}

func (m *Lifecycle) sync(key string, obj *v3.Node) (runtime.Object, error) {
	if obj == nil || obj.DeletionTimestamp != nil {
		return nil, nil
	}

	if cleanupAnnotation, ok := obj.Annotations[userNodeRemoveCleanupAnnotation]; !ok || cleanupAnnotation != "true" {
		// finalizer from user-node-remove has to be checked/cleaned
		return m.userNodeRemoveCleanup(obj)
	}

	return obj, nil
}

func (m *Lifecycle) Updated(obj *v3.Node) (runtime.Object, error) {
	newObj, err := v32.NodeConditionProvisioned.Once(obj, func() (runtime.Object, error) {
		if obj.Status.NodeTemplateSpec == nil {
			m.setWaiting(obj)
			return obj, nil
		}

		if !m.devMode {
			logrus.Infof("Creating jail for %v", obj.Namespace)
			err := jailer.CreateJail(obj.Namespace)
			if err != nil {
				return nil, errors.WithMessage(err, "node update jail error")
			}
		}

		obj, err := m.ready(obj)
		if err == nil {
			m.setWaiting(obj)
		}
		return obj, err
	})
	return newObj.(*v3.Node), err
}

func (m *Lifecycle) saveConfig(config *nodeconfig.NodeConfig, nodeDir string, obj *v3.Node) (*v3.Node, error) {
	logrus.Infof("Generating and uploading node config %s", obj.Spec.RequestedHostname)
	if err := config.Save(); err != nil {
		return obj, err
	}

	ip, err := config.IP()
	if err != nil {
		return obj, err
	}

	interalAddress, err := config.InternalIP()
	if err != nil {
		return obj, err
	}

	keyPath, err := config.SSHKeyPath()
	if err != nil {
		return obj, err
	}

	sshKey, err := getSSHKey(nodeDir, keyPath, obj)
	if err != nil {
		return obj, err
	}

	sshUser, err := config.SSHUser()
	if err != nil {
		return obj, err
	}

	if err := config.Save(); err != nil {
		return obj, err
	}

	template, err := m.getNodeTemplate(obj.Spec.NodeTemplateName)
	if err != nil {
		return obj, err
	}

	pool, err := m.getNodePool(obj.Spec.NodePoolName)
	if err != nil {
		return obj, err
	}

	obj.Status.NodeConfig = &rketypes.RKEConfigNode{
		NodeName:         obj.Namespace + ":" + obj.Name,
		Address:          ip,
		InternalAddress:  interalAddress,
		User:             sshUser,
		Role:             roles(obj),
		HostnameOverride: obj.Spec.RequestedHostname,
		SSHKey:           sshKey,
		Labels:           template.Labels,
	}
	obj.Status.InternalNodeStatus.Addresses = []v1.NodeAddress{
		{
			Type:    v1.NodeInternalIP,
			Address: obj.Status.NodeConfig.Address,
		},
	}

	if len(obj.Status.NodeConfig.Role) == 0 {
		obj.Status.NodeConfig.Role = []string{"worker"}
	}

	templateSet := taints.GetKeyEffectTaintSet(template.Spec.NodeTaints)
	nodeSet := taints.GetKeyEffectTaintSet(pool.Spec.NodeTaints)
	expectTaints := pool.Spec.NodeTaints

	for key, ti := range templateSet {
		// the expect taints are based on the node pool. so we don't need to set taints with same key and effect by template because
		// the taints from node pool should override the taints from template.
		if _, ok := nodeSet[key]; !ok {
			expectTaints = append(expectTaints, template.Spec.NodeTaints[ti])
		}
	}
	obj.Status.NodeConfig.Taints = taints.GetRKETaintsFromTaints(expectTaints)

	return obj, nil
}

func (m *Lifecycle) refreshNodeConfig(nc *nodeconfig.NodeConfig, obj *v3.Node) error {
	template, err := m.getNodeTemplate(obj.Spec.NodeTemplateName)
	if err != nil {
		return err
	}

	rawTemplate, err := m.nodeTemplateGenericClient.GetNamespaced(template.Namespace, template.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	data := rawTemplate.(*unstructured.Unstructured).Object
	rawConfig, ok := values.GetValue(data, template.Spec.Driver+"Config")
	if !ok {
		return fmt.Errorf("refreshNodeConfig: node config not specified for node %v", obj.Name)
	}

	if err := m.updateRawConfigFromCredential(data, rawConfig, template); err != nil {
		logrus.Debugf("refreshNodeConfig: error calling updateRawConfigFromCredential for [%v]: %v", obj.Name, err)
		return err
	}

	var update bool

	if template.Spec.Driver == amazonec2 {
		setEc2ClusterIDTag(rawConfig, obj.Namespace)
		logrus.Debug("refreshNodeConfig: Updating amazonec2 machine config")
		//TODO: Update to not be amazon specific, this needs to be moved to the driver
		update, err = nc.UpdateAmazonAuth(rawConfig)
		if err != nil {
			return err
		}
	}

	bytes, err := json.Marshal(rawConfig)
	if err != nil {
		return errors.Wrap(err, "failed to marshal node driver config")
	}

	newConfig := string(bytes)

	currentConfig, err := nc.DriverConfig()
	if err != nil {
		return err
	}

	if currentConfig != newConfig || update {
		err = nc.SetDriverConfig(string(bytes))
		if err != nil {
			return err
		}

		return nc.Save()
	}

	return nil
}

func (m *Lifecycle) isNodeInAppliedSpec(node *v3.Node) (bool, error) {
	// worker/controlplane nodes can just be immediately deleted
	if !node.Spec.Etcd {
		return false, nil
	}

	cluster, err := m.clusterLister.Get("", node.Namespace)
	if err != nil {
		if kerror.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if cluster == nil {
		return false, nil
	}
	if cluster.DeletionTimestamp != nil {
		return false, nil
	}
	if cluster.Status.AppliedSpec.RancherKubernetesEngineConfig == nil {
		return false, nil
	}

	for _, rkeNode := range cluster.Status.AppliedSpec.RancherKubernetesEngineConfig.Nodes {
		nodeName := rkeNode.NodeName
		if len(nodeName) == 0 {
			continue
		}
		if nodeName == fmt.Sprintf("%s:%s", node.Namespace, node.Name) {
			return true, nil
		}
	}
	return false, nil
}

func validateCustomHost(obj *v3.Node) error {
	if obj.Spec.Imported {
		return nil
	}

	customConfig := obj.Spec.CustomConfig
	signer, err := ssh.ParsePrivateKey([]byte(customConfig.SSHKey))
	if err != nil {
		return errors.Wrapf(err, "sshKey format is invalid")
	}
	config := &ssh.ClientConfig{
		User: customConfig.User,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	conn, err := ssh.Dial("tcp", customConfig.Address+":22", config)
	if err != nil {
		return errors.Wrapf(err, "Failed to validate ssh connection to address [%s]", customConfig.Address)
	}
	defer conn.Close()
	return nil
}

func roles(node *v3.Node) []string {
	var roles []string
	if node.Spec.Etcd {
		roles = append(roles, "etcd")
	}
	if node.Spec.ControlPlane {
		roles = append(roles, "controlplane")
	}
	if node.Spec.Worker {
		roles = append(roles, "worker")
	}
	if len(roles) == 0 {
		return []string{"worker"}
	}
	return roles
}

func (m *Lifecycle) setCredFields(data interface{}, fields map[string]v32.Field, credID string) error {
	splitID := strings.Split(credID, ":")
	if len(splitID) != 2 {
		return fmt.Errorf("invalid credential id %s", credID)
	}
	cred, err := m.credLister.Get(namespace.GlobalNamespace, splitID[1])
	if err != nil {
		return err
	}
	if ans := convert.ToMapInterface(data); len(ans) > 0 {
		for key, val := range cred.Data {
			splitKey := strings.Split(key, "-")
			if len(splitKey) == 2 && strings.HasSuffix(splitKey[0], "Config") {
				if _, ok := fields[splitKey[1]]; ok {
					ans[splitKey[1]] = string(val)
				}
			}
		}
	}
	return nil
}

func (m *Lifecycle) updateRawConfigFromCredential(data map[string]interface{}, rawConfig interface{}, template *v3.NodeTemplate) error {
	credID := convert.ToString(values.GetValueN(data, "spec", "cloudCredentialName"))
	if credID != "" {
		existingSchema, err := m.schemaLister.Get("", template.Spec.Driver+"config")
		if err != nil {
			return err
		}
		logrus.Debugf("setCredFields for credentialName %s", credID)
		err = m.setCredFields(rawConfig, existingSchema.Spec.ResourceFields, credID)
		if err != nil {
			return errors.Wrap(err, "failed to set credential fields")
		}
	}
	return nil
}
