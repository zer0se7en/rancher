package clusterregistrationtoken

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/rancher/norman/types/convert"
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	util "github.com/rancher/rancher/pkg/cluster"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/image"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/systemtemplate"
	rketypes "github.com/rancher/rke/types"
)

const (
	commandFormat                 = "kubectl apply -f %s"
	insecureCommandFormat         = "curl --insecure -sfL %s | kubectl apply -f -"
	nodeCommandFormat             = "sudo docker run -d --privileged --restart=unless-stopped --net=host -v /etc/kubernetes:/etc/kubernetes -v /var/run:/var/run %s %s --server %s --token %s%s"
	shareMntCommandFormat         = "agent --node-name %s --server %s --token %s%s --no-register --only-write-certs"
	rke2NodeCommandFormat         = "curl -fL %s | sudo %s sh -s - --server %s --token %s%s"
	rke2InsecureNodeCommandFormat = "curl --insecure -fL %s | sudo %s sh -s - --server %s --token %s%s"
	loginCommandFormat            = "echo \"%s\" | sudo docker login --username %s --password-stdin %s"
	windowsNodeCommandFormat      = `PowerShell -NoLogo -NonInteractive -Command "& {docker run -v c:\:c:\host %s%s bootstrap --server %s --token %s%s%s | iex}"`
)

func (h *handler) isRKE2(clusterID string) bool {
	cluster, err := h.clusters.Get(clusterID)
	if err != nil {
		return false
	}
	return cluster.Annotations["objectset.rio.cattle.io/owner-gvk"] == "provisioning.cattle.io/v1, Kind=Cluster"
}

func (h *handler) assignStatus(crt *v32.ClusterRegistrationToken) (v32.ClusterRegistrationTokenStatus, error) {
	ca := systemtemplate.CAChecksum()
	if ca != "" {
		ca = " --ca-checksum " + ca
	}

	token := crt.Status.Token
	clusterID := convert.ToString(crt.Spec.ClusterName)
	if token == "" {
		return crt.Status, nil
	}

	crtStatus := crt.Status.DeepCopy()
	crtStatus.Token = token

	url, err := getURL(token, clusterID)
	if err != nil {
		return crt.Status, err
	}

	if url == "" {
		return *crtStatus, nil
	}

	crtStatus.InsecureCommand = fmt.Sprintf(insecureCommandFormat, url)
	crtStatus.Command = fmt.Sprintf(commandFormat, url)
	crtStatus.ManifestURL = url

	rootURL, err := getRootURL()
	if err != nil {
		return crt.Status, err
	}

	cluster, err := h.clusters.Get(clusterID)
	if err != nil {
		return crt.Status, err
	}

	agentImage := image.ResolveWithCluster(settings.AgentImage.Get(), cluster)
	if h.isRKE2(clusterID) {
		// for linux
		crtStatus.NodeCommand = fmt.Sprintf(rke2NodeCommandFormat,
			rootURL+"/system-agent-install.sh",
			AgentEnvVars(cluster, false),
			rootURL,
			token,
			ca)
		crtStatus.InsecureNodeCommand = fmt.Sprintf(rke2InsecureNodeCommandFormat,
			rootURL+"/system-agent-install.sh",
			AgentEnvVars(cluster, false),
			rootURL,
			token,
			ca)
	} else {
		// for linux
		crtStatus.NodeCommand = fmt.Sprintf(nodeCommandFormat,
			AgentEnvVars(cluster, true),
			agentImage,
			rootURL,
			token,
			ca)
	}
	// for windows
	var agentImageDockerEnv string
	if util.GetPrivateRepoURL(cluster) != "" {
		// patch the AGENT_IMAGE env
		agentImageDockerEnv = fmt.Sprintf("-e AGENT_IMAGE=%s ", agentImage)
	}
	crtStatus.WindowsNodeCommand = fmt.Sprintf(windowsNodeCommandFormat,
		agentImageDockerEnv,
		agentImage,
		rootURL,
		token,
		ca,
		getWindowsPrefixPathArg(cluster.Spec.RancherKubernetesEngineConfig))

	return *crtStatus, nil
}

func getWindowsPrefixPathArg(rkeConfig *rketypes.RancherKubernetesEngineConfig) string {
	if rkeConfig == nil {
		return ""
	}
	// default to prefix path
	prefixPath := rkeConfig.PrefixPath

	// if windows prefix path set, override
	if rkeConfig.WindowsPrefixPath != "" {
		prefixPath = rkeConfig.WindowsPrefixPath
	}

	if prefixPath != "" {
		return fmt.Sprintf(" --prefix-path %s", prefixPath)
	}

	return ""
}

func AgentEnvVars(cluster *v3.Cluster, docker bool) string {
	var agentEnvVars []string
	if cluster != nil {
		for _, envVar := range cluster.Spec.AgentEnvVars {
			if envVar.Value != "" {
				if docker {
					agentEnvVars = append(agentEnvVars, fmt.Sprintf("-e \"%s=%s\"", envVar.Name, envVar.Value))
				} else {
					agentEnvVars = append(agentEnvVars, fmt.Sprintf("%s=\"%s\"", envVar.Name, envVar.Value))
				}
			}
		}
	}
	return strings.Join(agentEnvVars, " ")
}

func NodeCommand(token string, cluster *v3.Cluster) (string, error) {
	ca := systemtemplate.CAChecksum()
	if ca != "" {
		ca = " --ca-checksum " + ca
	}

	rootURL, err := getRootURL()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(nodeCommandFormat,
		AgentEnvVars(cluster, true),
		image.ResolveWithCluster(settings.AgentImage.Get(), cluster),
		rootURL,
		token,
		ca), nil
}

func ShareMntCommand(nodeName, token string, cluster *v3.Cluster) ([]string, error) {
	rootURL, err := getRootURL()
	if err != nil {
		return []string{""}, err
	}

	cmd := []string{
		"--no-register", "--only-write-certs",
		"--node-name", nodeName,
		"--server", rootURL,
		"--token", token,
	}

	ca := systemtemplate.CAChecksum()
	if ca != "" {
		cmd = append(cmd, fmt.Sprintf("--ca-checksum %s", ca))
	}

	return cmd, nil
}

func LoginCommand(reg rketypes.PrivateRegistry) string {
	return fmt.Sprintf(
		loginCommandFormat,
		// escape password special characters so it is interpreted correctly when command is executed
		escapeSpecialChars(reg.Password),
		reg.User,
		reg.URL,
	)
}

// escapeSpecialChars escapes ", `, $, \ from a string s
func escapeSpecialChars(s string) string {
	var escaped []rune
	for _, r := range s {
		switch r {
		case '"', '`', '$', '\\': // escape
			escaped = append(escaped, '\\', r)
		default: // no escape
			escaped = append(escaped, r)
		}
	}
	return string(escaped)
}

func getRootURL() (string, error) {
	serverURL := settings.ServerURL.Get()
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}

	u.Path = ""
	return u.String(), nil
}

func getURL(token, clusterID string) (string, error) {
	serverURL := settings.ServerURL.Get()
	if serverURL == "" {
		return "", nil
	}
	path := "/v3/import/" + token + "_" + clusterID + ".yaml"
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}

	u.Path = path
	serverURL = u.String()
	return serverURL, nil
}
