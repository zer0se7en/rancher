package machineprovision

import (
	name2 "github.com/rancher/wrangler/pkg/name"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	InfraMachineGroup   = "rke.cattle.io/infra-machine-group"
	InfraMachineVersion = "rke.cattle.io/infra-machine-version"
	InfraMachineKind    = "rke.cattle.io/infra-machine-kind"
	InfraMachineName    = "rke.cattle.io/infra-machine-name"

	pathToMachineFiles = "/path/to/machine/files"
)

func getJobName(name string) string {
	return name2.SafeConcatName(name, "machine", "provision")
}

func (h *handler) objects(ready bool, typeMeta metav1.Type, meta metav1.Object, args driverArgs, filesSecret *corev1.Secret) ([]runtime.Object, error) {
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount
	machineGVK := schema.FromAPIVersionAndKind(typeMeta.GetAPIVersion(), typeMeta.GetKind())
	saName := getJobName(meta.GetName())
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      args.StateSecretName,
			Namespace: meta.GetNamespace(),
		},
		Type: "rke.cattle.io/machine-state",
	}

	if ready {
		return []runtime.Object{secret}, nil
	}

	if args.BootstrapOptional && args.BootstrapSecretName == "" {
		args.BootstrapSecretName = "not-found"
	}

	if filesSecret != nil {
		if filesSecret.Name == "" {
			filesSecret.Name = saName
			filesSecret.Namespace = meta.GetNamespace()
		}

		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "machine-files",
			ReadOnly:  true,
			MountPath: pathToMachineFiles,
		})

		keysToPaths := make([]corev1.KeyToPath, 0, len(filesSecret.Data))
		for file := range filesSecret.Data {
			keysToPaths = append(keysToPaths, corev1.KeyToPath{Key: file, Path: file})
		}
		volumes = append(volumes, corev1.Volume{
			Name: "machine-files",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  filesSecret.Name,
					Items:       keysToPaths,
					DefaultMode: &[]int32{0600}[0],
				},
			},
		})
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: meta.GetNamespace(),
		},
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: meta.GetNamespace(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:         []string{"get", "update"},
				APIGroups:     []string{""},
				Resources:     []string{"secrets"},
				ResourceNames: []string{secret.Name},
			},
		},
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: meta.GetNamespace(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: meta.GetNamespace(),
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     saName,
		},
	}
	rb2 := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name2.SafeConcatName(saName, "extension"),
			Namespace: meta.GetNamespace(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: meta.GetNamespace(),
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     "rke2-machine-provisioner",
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: meta.GetNamespace(),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &[]int32{0}[0],
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						InfraMachineGroup:   machineGVK.Group,
						InfraMachineVersion: machineGVK.Version,
						InfraMachineKind:    machineGVK.Kind,
						InfraMachineName:    meta.GetName(),
					},
				},
				Spec: corev1.PodSpec{
					Volumes: append(volumes, []corev1.Volume{
						{
							Name: "bootstrap",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  args.BootstrapSecretName,
									DefaultMode: &[]int32{0700}[0],
									Optional:    &args.BootstrapOptional,
								},
							},
						},
						{
							Name: "tls-ca-additional-volume",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  "tls-ca-additional-volume",
									DefaultMode: &[]int32{0400}[0],
									Optional:    &[]bool{true}[0],
								},
							},
						},
					}...),
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "machine",
							Image:           args.ImageName,
							ImagePullPolicy: args.ImagePullPolicy,
							Args:            args.Args,
							EnvFrom: []corev1.EnvFromSource{
								{
									SecretRef: &corev1.SecretEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: args.EnvSecret.Name,
										},
									},
								},
							},
							VolumeMounts: append(volumeMounts, []corev1.VolumeMount{
								{
									Name:      "bootstrap",
									ReadOnly:  false,
									MountPath: "/run/secrets/machine",
								},
								{
									Name:      "tls-ca-additional-volume",
									ReadOnly:  true,
									MountPath: "/etc/ssl/certs/ca-additional.pem",
									SubPath:   "ca-additional.pem",
								},
							}...),
						},
					},
					ServiceAccountName: saName,
				},
			},
		},
	}

	return []runtime.Object{
		args.EnvSecret,
		secret,
		sa,
		role,
		rb,
		filesSecret,
		rb2,
		job,
	}, nil
}
