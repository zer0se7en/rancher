package configserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	machineRequestType = "rke.cattle.io/machine-request"
	machineIDHeader    = "X-Cattle-Id"
	headerPrefix       = "X-Cattle-"
)

func (r *RKE2ConfigServer) findMachineByClusterToken(req *http.Request) (string, string, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		return "", "", nil
	}

	machineID := req.Header.Get(machineIDHeader)
	if machineID == "" {
		return "", "", nil
	}

	tokens, err := r.clusterTokenCache.GetByIndex(tokenIndex, token)
	if err != nil {
		return "", "", err
	}

	data := map[string]interface{}{}
	for k, v := range req.Header {
		if strings.HasPrefix(k, headerPrefix) {
			data[strings.ToLower(strings.TrimPrefix(k, headerPrefix))] = v
		}
	}
	data["id"] = machineID

	if len(tokens) == 0 {
		return "", "", nil
	}

	secretName := machineRequestSecretName(machineID)
	secret, err := r.secretsCache.Get(tokens[0].Namespace, secretName)
	if apierror.IsNotFound(err) {
		secret, err = r.createSecret(tokens[0].Namespace, secretName, data)
	}
	if err != nil {
		return "", "", err
	}

	secret, err = r.waitReady(secret)
	if err != nil {
		return "", "", err
	}

	machineNamespace, machineName := secret.Labels[machineNamespaceLabel], secret.Labels[machineNameLabel]
	_ = r.secrets.Delete(secret.Namespace, secret.Name, nil)
	return machineNamespace, machineName, nil
}

func (r *RKE2ConfigServer) createSecret(namespace, name string, data map[string]interface{}) (*corev1.Secret, error) {
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	return r.secrets.Create(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Immutable: nil,
		Data: map[string][]byte{
			"data": dataBytes,
		},
		Type: machineRequestType,
	})
}

func (r *RKE2ConfigServer) waitReady(secret *corev1.Secret) (*corev1.Secret, error) {
	if secret.Labels[machineNameLabel] != "" {
		return secret, nil
	}

	resp, err := r.secrets.Watch(secret.Namespace, metav1.ListOptions{
		TimeoutSeconds: &[]int64{120}[0],
		FieldSelector:  "metadata.name=" + secret.Name,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		resp.Stop()
		for range resp.ResultChan() {
		}
	}()

	for obj := range resp.ResultChan() {
		secret, ok := obj.Object.(*corev1.Secret)
		if ok && secret.Labels[machineNameLabel] != "" {
			return secret, nil
		}
	}

	return nil, fmt.Errorf("timeout waiting for %s/%s to be ready", secret.Namespace, secret.Name)
}

func machineRequestSecretName(name string) string {
	hash := sha256.Sum256([]byte(name))
	return "custom-" + hex.EncodeToString(hash[:])[:12]
}
