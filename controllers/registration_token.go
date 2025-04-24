package controllers

import (
	"context"
	"os"

	"github.com/rancher/lasso/pkg/log"
	"github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	controllersManagement "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/apps"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	coreAppisType "k8s.io/api/apps/v1"
	coreType "k8s.io/api/core/v1"
	errorsk8s "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"k8s.io/apimachinery/pkg/api/resource"
)

func InitRegistrationToken(ctx context.Context, mgmtManagement *controllersManagement.Factory, mgmtCore *core.Factory, mgmtApps *apps.Factory) {
	RegistrationTokenResourceController := mgmtManagement.Management().V3().ClusterRegistrationToken()
	CattleClusterResourceController := mgmtManagement.Management().V3().Cluster()
	SettingResourceController := mgmtManagement.Management().V3().Setting()
	SecretResourceController := mgmtCore.Core().V1().Secret()
	RegistrationTokenResourceController.OnChange(ctx, "gorizond-cluster-registration", func(key string, token *v3.ClusterRegistrationToken) (*v3.ClusterRegistrationToken, error) {
		if token == nil {
			return nil, nil
		}
		if token.Namespace == "local" {
			return token, nil
		}

		recordedNote := token.Annotations != nil && token.Annotations["cattle-cluster-agent-create"] == "true"

		if recordedNote {
			// there already is a note, noop
			return token, nil
		}
		if token.Status.Token == "" || token.Spec.ClusterName == "" {
			return token, nil
		}
		if token.Name != "default-token" {
			return token, nil
		}
		cluster, err := CattleClusterResourceController.Get(token.Namespace, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		serverUrl, err := SettingResourceController.Get("server-url", metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		agentImage, err := SettingResourceController.Get("agent-image", metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		serverVersion, err := SettingResourceController.Get("server-version", metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		secret := &coreType.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cluster.Spec.DisplayName + "-cattle",
				Namespace: cluster.Spec.FleetWorkspaceName,
				Labels: map[string]string{
					"gorizond-deploy": cluster.Spec.DisplayName,
				},
			},
			Data: map[string][]byte{
				"token":     []byte(token.Status.Token),
				"namespace": []byte(token.Namespace),
				"url":       []byte(serverUrl.Value),
			},
		}
		// Create the Secret in Kubernetes
		_, err = SecretResourceController.Create(secret)
		if err != nil {
			if errorsk8s.IsAlreadyExists(err) {
				log.Infof("Secret cluster agent %s already exists in namespace %s", secret.Name, secret.Namespace)
			} else {
				return nil, err
			}
		}

		deployment := coreAppisType.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cluster.Spec.DisplayName + "-agent",
				Namespace: cluster.Spec.FleetWorkspaceName,
				Labels: map[string]string{
					"app":             cluster.Spec.DisplayName + "-agent",
					"gorizond-deploy": cluster.Spec.DisplayName,
				},
			},
			Spec: coreAppisType.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": cluster.Spec.DisplayName + "-agent",
					},
				},
				Template: coreType.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": cluster.Spec.DisplayName + "-agent",
						},
					},
					Spec: coreType.PodSpec{
						AutomountServiceAccountToken: pointer.BoolPtr(false),
						Containers: []coreType.Container{
							{
								Name:            "cluster-register",
								Image:           agentImage.Value,
								ImagePullPolicy: coreType.PullIfNotPresent,
								Command: []string{
									"/bin/sh", "-c",
								},
								Resources: coreType.ResourceRequirements{
									Requests: coreType.ResourceList{
										coreType.ResourceCPU: resource.MustParse("10m"),
										coreType.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
								Args: []string{
									`curl --output /var/run/secrets/kubernetes.io/serviceaccount/ca.crt ` + cluster.Spec.DisplayName + "-k3s." + cluster.Spec.FleetWorkspaceName + `:80/ca.crt
kubectl -n cattle-system get secret cattle-admin-token -o jsonpath='{.data.token}' | base64 --decode > /var/run/secrets/kubernetes.io/serviceaccount/token
run.sh`,
								},
								Env: []coreType.EnvVar{
									{
										Name:  "CATTLE_IS_RKE",
										Value: "false",
									},
									{
										Name:  "CATTLE_SERVER",
										Value: serverUrl.Value,
									},
									{
										Name:  "CATTLE_CA_CHECKSUM",
										Value: os.Getenv("CATTLE_CA_CHECKSUM"),
									},
									{
										Name:  "CATTLE_CLUSTER",
										Value: "true",
									},
									{
										Name:  "CATTLE_K8S_MANAGED",
										Value: "true",
									},
									{
										Name:  "CATTLE_CLUSTER_REGISTRY",
										Value: "",
									},
									{
										Name:  "CATTLE_SERVER_VERSION",
										Value: serverVersion.Value,
									},
									{
										Name:  "CATTLE_INGRESS_IP_DOMAIN",
										Value: "sslip.io",
									},
									{
										Name:  "STRICT_VERIFY",
										Value: "true",
									},
									{
										Name:  "KUBECONFIG",
										Value: "/root/.kube/value",
									},
									{
										Name:  "KUBERNETES_SERVICE_HOST",
										Value: cluster.Spec.DisplayName + "-k3s." + cluster.Spec.FleetWorkspaceName,
									},
									{
										Name:  "KUBERNETES_SERVICE_PORT",
										Value: "6443",
									},
								},
								Ports: []coreType.ContainerPort{
									{
										ContainerPort: 80,
										Protocol:      coreType.ProtocolTCP,
									},
								},
								VolumeMounts: []coreType.VolumeMount{
									{
										Name:      "cattle-credentials",
										MountPath: "/cattle-credentials",
									},
									{
										Name:      "k3s-config",
										MountPath: "/root/.kube",
									},
									{
										Name:      "serviceaccount",
										MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
									},
								},
							},
						},
						Volumes: []coreType.Volume{
							{
								Name: "cattle-credentials",
								VolumeSource: coreType.VolumeSource{
									Secret: &coreType.SecretVolumeSource{
										SecretName: cluster.Spec.DisplayName + "-cattle",
									},
								},
							},
							{
								Name: "k3s-config",
								VolumeSource: coreType.VolumeSource{
									Secret: &coreType.SecretVolumeSource{
										SecretName: cluster.Spec.DisplayName + "-kubeconfig",
									},
								},
							},
							{
								Name: "serviceaccount",
								VolumeSource: coreType.VolumeSource{
									EmptyDir: &coreType.EmptyDirVolumeSource{},
								},
							},
						},
					},
				},
			},
		}

		_, err = mgmtApps.Apps().V1().Deployment().Create(&deployment)
		if err != nil {
			if errorsk8s.IsAlreadyExists(err) {
				log.Infof("Deployment %s already exists in namespace %s", deployment.Name, deployment.Namespace)
			} else {
				return nil, err
			}
		}
		if token.Annotations == nil {
			token.Annotations = make(map[string]string)
		}

		token.Annotations["cattle-cluster-agent-create"] = "true"
		return RegistrationTokenResourceController.Update(token)
	})
}
