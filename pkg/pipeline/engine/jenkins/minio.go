package jenkins

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	v3 "github.com/rancher/rancher/pkg/generated/norman/project.cattle.io/v3"
	"github.com/rancher/rancher/pkg/pipeline/utils"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type minioClient struct {
	client minio.Client
}

func (j *Engine) getMinioURL(ns string) (string, error) {
	MinioName := utils.MinioName
	svc, err := j.ServiceLister.Get(ns, MinioName)
	if err != nil {
		return "", err
	}

	ip := svc.Spec.ClusterIP

	return fmt.Sprintf("%s:%d", ip, utils.MinioPort), nil
}

func (j *Engine) getMinioClient(ns string) (*minio.Client, error) {
	url, err := j.getMinioURL(ns)
	if err != nil {
		return nil, err
	}

	user := utils.PipelineSecretDefaultUser
	var secret *corev1.Secret
	if j.UseCache {
		secret, err = j.SecretLister.Get(ns, utils.PipelineSecretName)
	} else {
		secret, err = j.Secrets.GetNamespaced(ns, utils.PipelineSecretName, metav1.GetOptions{})
	}
	if err != nil || secret.Data == nil {
		return nil, fmt.Errorf("error get minio token - %v", err)
	}
	token := string(secret.Data[utils.PipelineSecretTokenKey])

	// https://github.com/minio/minio-go/blob/0be3a44757352b6e617ef00eb47829bce29baab1/api.go#L153
	opt := minio.Options{
		Creds:        credentials.NewStaticV4(user, token, ""),
		Secure:       false,
		Region:       "",
		BucketLookup: minio.BucketLookupAuto,
	}

	if j.HTTPClient == nil {
		dial, err := j.Dialer.ClusterDialer(j.ClusterName)
		if err != nil {
			return nil, err
		}
		j.HTTPClient = &http.Client{
			Transport: &http.Transport{
				DialContext: dial,
			},
			Timeout: 15 * time.Second,
		}
	}
	opt.Transport = j.HTTPClient.Transport
	client, err := minio.New(url, &opt)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (j Engine) getStepLogFromMinioStore(execution *v3.PipelineExecution, stage int, step int) (string, error) {
	bucketName := utils.MinioLogBucket
	logName := fmt.Sprintf("%s-%d-%d", execution.Name, stage, step)
	ns := utils.GetPipelineCommonName(execution.Spec.ProjectName)
	client, err := j.getMinioClient(ns)
	if err != nil {
		return "", err
	}

	reader, err := client.GetObject(context.TODO(), bucketName, logName, minio.GetObjectOptions{})

	//stat, err := reader.Stat()
	if err != nil {
		return "", err
	}

	content, err := ioutil.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func (j *Engine) saveStepLogToMinio(execution *v3.PipelineExecution, stage int, step int) error {
	bucketName := utils.MinioLogBucket
	logName := fmt.Sprintf("%s-%d-%d", execution.Name, stage, step)
	ns := utils.GetPipelineCommonName(execution.Spec.ProjectName)
	client, err := j.getMinioClient(ns)
	if err != nil {
		return err
	}
	//Make Bucket
	exists, err := client.BucketExists(context.TODO(), bucketName)
	if err != nil {
		logrus.Error(err)
	}
	if !exists {
		makeBucketOpt := minio.MakeBucketOptions{Region: utils.MinioBucketLocation}
		if err := client.MakeBucket(context.TODO(), bucketName, makeBucketOpt); err != nil {
			return err
		}
	}

	message, err := j.getStepLogFromJenkins(execution, stage, step)
	if err != nil {
		return err
	}

	_, err = client.PutObject(context.TODO(), bucketName, logName, strings.NewReader(message), int64(len(message)), minio.PutObjectOptions{})
	return err
}
