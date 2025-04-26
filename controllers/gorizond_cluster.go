package controllers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorizond/gorizond-cluster/pkg"
	networkingv1 "github.com/gorizond/gorizond-cluster/pkg/apis/networking.k8s.io/v1"
	gorizondv1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
	controllersIngress "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/networking.k8s.io"
	controllersIngressv1 "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/networking.k8s.io/v1"
	controllers "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	controllersv1 "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io/v1"
	headscalev1 "github.com/juanfont/headscale/gen/go/headscale/v1"
	"github.com/kos-v/dsnparser"
	"github.com/rancher/lasso/pkg/log"
	cattlev1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	controllersProvision "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/apps"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	corev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
	coreAppisType "k8s.io/api/apps/v1"
	coreType "k8s.io/api/core/v1"
	errorsk8s "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	"tailscale.com/types/key"
)

func InitClusterController(ctx context.Context, mgmtGorizond *controllers.Factory, mgmtProvision *controllersProvision.Factory, mgmtCore *core.Factory, mgmtApps *apps.Factory, mgmtNetwork *controllersIngress.Factory, dbHeadScale *pkg.DatabaseManager, dbKubernetes *pkg.DatabaseManager) {
	GorizondResourceController := mgmtGorizond.Provisioning().V1().Cluster()
	SecretResourceController := mgmtCore.Core().V1().Secret()
	NamespaceResourceController := mgmtCore.Core().V1().Namespace()
	ProvisionResourceController := mgmtProvision.Provisioning().V1().Cluster()
	NetworkResourceController := mgmtNetwork.Networking().V1().Ingress()
	dsnHeadScale := dsnparser.Parse(os.Getenv("DB_DSN_HEADSCALE"))
	ProvisionResourceController.OnChange(ctx, "status-for-gorizond-cluster", func(key string, obj *cattlev1.Cluster) (*cattlev1.Cluster, error) {
		if obj == nil {
			return nil, nil
		}
		if obj.Name == "local" && obj.Namespace == "fleet-local" {
			return nil, nil
		}
		gorizondClusterBind := obj.Annotations != nil && obj.Annotations["gorizond-cluster-bind"] == "true"

		if gorizondClusterBind {
			// there already is a note, noop
			return obj, nil
		}
		gotrizondCluster, err := mgmtGorizond.Provisioning().V1().Cluster().Get(obj.Namespace, obj.Name, metav1.GetOptions{})
		if err != nil {
			return obj, nil
		}
		if obj.Status.ClusterName == "" {
			return obj, nil
		}
		gotrizondCluster.Status.Cluster = obj.Status.ClusterName
		_, err = GorizondResourceController.Update(gotrizondCluster)
		if err != nil {
			return obj, err
		}

		if obj.Annotations == nil {
			obj.Annotations = make(map[string]string)
		}

		obj.Annotations["gorizond-cluster-bind"] = "true"
		return ProvisionResourceController.Update(obj)
	})
	GorizondResourceController.OnChange(ctx, "gorizond-create-cluster", func(key string, obj *gorizondv1.Cluster) (*gorizondv1.Cluster, error) {
		if obj == nil {
			return nil, nil
		}

		if obj.Status.Provisioning == "Done" {
			return nil, nil
		}

		if obj.Namespace == "fleet-default" || obj.Namespace == "fleet-local" {
			return nil, nil
		}
		
		// create Namespace in cluster if not exist
		if obj.Status.Namespace == "" {
			_, err := NamespaceResourceController.Create(&coreType.Namespace{ObjectMeta: metav1.ObjectMeta{Name: obj.Namespace}})
			if err != nil {
				if errorsk8s.IsAlreadyExists(err) {
					log.Infof("Namespace %s already exists", obj.Namespace)
				} else {
					return nil, err
				}
			}
			obj.Status.Namespace = obj.Namespace
			return GorizondResourceController.Update(obj)
		}

		if obj.Status.Provisioning == "" {
			return createCattleCluster(obj, mgmtProvision, GorizondResourceController)
		}

		if obj.Status.Cluster != "" {
			sanitizedNameHs := strings.NewReplacer(
				".", "_",
				"-", "_",
				" ", "_",
				"@", "_",
				"#", "_",
				"$", "_",
			).Replace(obj.Namespace + "_hs_" + obj.Status.Cluster)
			sanitizedNameApi := strings.NewReplacer(
				".", "_",
				"-", "_",
				" ", "_",
				"@", "_",
				"#", "_",
				"$", "_",
			).Replace(obj.Namespace + "_api_" + obj.Status.Cluster)
			// check version
			version, err := obj.ValidateKubernetesVersion()
			if err != nil {
				return nil, err
			}
			if version != obj.Spec.KubernetesVersion {
				obj.Spec.KubernetesVersion = version
				return GorizondResourceController.Update(obj)
			}

			if obj.Status.Provisioning == "WaitHeadScaleDatabase" {
				return createHeadScaleDatabase(sanitizedNameHs, obj, dbHeadScale, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken == "" && obj.Status.Provisioning == "WaitHeadScaleConfig" {
				return createHeadScaleConfig(sanitizedNameHs, obj, dsnHeadScale, SecretResourceController, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken == "" && obj.Status.Provisioning == "WaitHeadScaleCreate" {
				return createHeadScaleCreate(obj, mgmtCore, mgmtApps, NetworkResourceController, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken == "" && obj.Status.Provisioning == "WaitHeadScaleCreateUser" {
				return createHeadScaleCreateUser(ctx, obj, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken == "" && obj.Status.Provisioning == "WaitHeadScaleToken" {
				return createHeadScaleToken(ctx, obj, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken != "" && obj.Status.K3sToken == "" && obj.Status.Provisioning == "WaitKubernetesStorage" {
				return createKubernetesStorage(sanitizedNameApi, obj, dbKubernetes, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken != "" && obj.Status.K3sToken == "" && obj.Status.Provisioning == "WaitKubernetesDeploy" {
				return createKubernetesCreate(sanitizedNameApi, obj, mgmtCore, mgmtApps, SecretResourceController, NetworkResourceController, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken != "" && obj.Status.K3sToken == "" && obj.Status.Provisioning == "WaitKubernetesToken" {
				return createKubernetesToken(obj, SecretResourceController, GorizondResourceController)
			}

		}
		return obj, nil
	})
	GorizondResourceController.OnRemove(ctx, "gorizond-delete", func(key string, obj *gorizondv1.Cluster) (*gorizondv1.Cluster, error) {
		selector := fmt.Sprintf("gorizond-deploy=%s", obj.Name)
		clusters, err := mgmtProvision.Provisioning().V1().Cluster().List(obj.Namespace, metav1.ListOptions{})
		if err != nil {
			return obj, nil
		}
		for _, cluster := range clusters.Items {
			// list all Clusters in workspace and remove if gorziond.Name == cluster.Name
			if cluster.Name == obj.Name {
				err = mgmtProvision.Provisioning().V1().Cluster().Delete(obj.Namespace, cluster.Name, nil)
				if err != nil {
					return obj, nil
				}
				log.Infof("deleted cluster %s (%s)", cluster.Name, obj.Namespace)
			}
		}

		deployments, err := mgmtApps.Apps().V1().Deployment().List(obj.Namespace, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return obj, nil
		}
		for _, deployment := range deployments.Items {
			err = mgmtApps.Apps().V1().Deployment().Delete(obj.Namespace, deployment.Name, nil)
			if err != nil {
				return obj, nil
			}
			log.Infof("deleted deployment %s (%s)", deployment.Name, obj.Namespace)
		}

		secrets, err := SecretResourceController.List(obj.Namespace, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return obj, nil
		}
		for _, secret := range secrets.Items {
			err = SecretResourceController.Delete(obj.Namespace, secret.Name, nil)
			if err != nil {
				return obj, nil
			}
			log.Infof("deleted secret %s (%s)", secret.Name, obj.Namespace)
		}
		ingresses, err := mgmtNetwork.Networking().V1().Ingress().List(obj.Namespace, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return obj, nil
		}
		for _, ingress := range ingresses.Items {
			err = mgmtNetwork.Networking().V1().Ingress().Delete(obj.Namespace, ingress.Name, nil)
			if err != nil {
				return obj, nil
			}
			log.Infof("deleted ingress %s (%s)", ingress.Name, obj.Namespace)
		}

		services, err := mgmtCore.Core().V1().Service().List(obj.Namespace, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return obj, nil
		}
		for _, service := range services.Items {
			err = mgmtCore.Core().V1().Service().Delete(obj.Namespace, service.Name, nil)
			if err != nil {
				return obj, nil
			}
			log.Infof("deleted service %s (%s)", service.Name, obj.Namespace)
		}

		if obj.Status.Cluster != "" {
			sanitizedNameHs := strings.NewReplacer(
				".", "_",
				"-", "_",
				" ", "_",
				"@", "_",
				"#", "_",
				"$", "_").Replace(obj.Namespace + "_hs_" + obj.Status.Cluster)
			sanitizedNameApi := strings.NewReplacer(
				".", "_",
				"-", "_",
				" ", "_",
				"@", "_",
				"#", "_",
				"$", "_").Replace(obj.Namespace + "_api_" + obj.Status.Cluster)

			err = dbHeadScale.AddDatabaseToRemove(sanitizedNameHs)
			if err != nil {
				return nil, err
			}
			err = dbKubernetes.AddDatabaseToRemove(sanitizedNameApi)
			if err != nil {
				return nil, err
			}
		}
		log.Infof("Successfully remove: %s.%s", obj.Namespace, obj.Name)
		return obj, nil
	})
}

func createKubernetesToken(obj *gorizondv1.Cluster, SecretResourceController corev1.SecretController, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {
	curlTarget := "http://" + obj.Name + "-k3s." + obj.Namespace + ".svc:80"

	// get k3s token
	var data string
	timeout := time.Minute * 2
	start := time.Now()

	for time.Since(start) < timeout {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(curlTarget + "/token")
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		result, err := handleResponse(resp)
		if err == nil {
			data = result
			break
		}

		time.Sleep(1 * time.Second)
	}

	// get k3s.yaml
	var k3sYaml string
	timeout = time.Minute * 2
	start = time.Now()

	for time.Since(start) < timeout {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(curlTarget + "/k3s.yaml")
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		result, err := handleResponse(resp)
		if err == nil {
			k3sYaml = result
			break
		}

		time.Sleep(1 * time.Second)
	}

	secret := &coreType.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-kubeconfig",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"gorizond-deploy": obj.Name,
			},
		},
		Data: map[string][]byte{
			"value": []byte(k3sYaml),
		},
	}
	// Create the Secret in Kubernetes
	_, err := SecretResourceController.Create(secret)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Secret k3s %s already exists in namespace %s", secret.Name, secret.Namespace)
		} else {
			return nil, err
		}
	}

	if data != "" {
		obj.Status.K3sToken = data
		obj.Status.Provisioning = "Done"
		return GorizondResourceController.Update(obj)
	} else {
		return nil, fmt.Errorf("Kubernetes API not start in 2 minutes")
	}
}

func handleResponse(resp *http.Response) (string, error) {
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP статус: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения тела ответа: %v", err)
	}

	return string(body), nil
}

func createKubernetesCreate(sanitizedNameApi string, obj *gorizondv1.Cluster, mgmtCore *core.Factory, mgmtApps *apps.Factory, SecretResourceController corev1.SecretController, NetworkResourceController controllersIngressv1.IngressController , GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {

	secret := &coreType.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-manifest",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"gorizond-deploy": obj.Name,
			},
		},
		Data: map[string][]byte{
			"cattle-system-manifest.yaml": []byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: proxy-clusterrole-kubeapiserver
rules:
- apiGroups: [""]
  resources:
  - nodes/metrics
  - nodes/proxy
  - nodes/stats
  - nodes/log
  - nodes/spec
  verbs: ["get", "list", "watch", "create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: proxy-role-binding-kubernetes-master
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: proxy-clusterrole-kubeapiserver
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: User
  name: kube-apiserver
---
apiVersion: v1
kind: Namespace
metadata:
  name: cattle-system
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cattle
  namespace: cattle-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cattle-admin-binding
  namespace: cattle-system
  labels:
    cattle.io/creator: "norman"
subjects:
- kind: ServiceAccount
  name: cattle
  namespace: cattle-system
roleRef:
  kind: ClusterRole
  name: cattle-admin
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cattle-admin
  labels:
    cattle.io/creator: "norman"
rules:
- apiGroups:
  - '*'
  resources:
  - '*'
  verbs:
  - '*'
- nonResourceURLs:
  - '*'
  verbs:
  - '*'
---
apiVersion: v1
kind: Secret
metadata:
  name: cattle-admin-token
  namespace: cattle-system
  annotations:
    kubernetes.io/service-account.name: cattle
type: kubernetes.io/service-account-token`),
		},
	}
	// Create the Secret in Kubernetes
	_, err := SecretResourceController.Create(secret)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Secret manifest %s already exists in namespace %s", secret.Name, secret.Namespace)
		} else {
			return nil, err
		}
	}

	domain := obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster + "." + os.Getenv("GORIZOND_DOMAIN_K3S")
	hostPathType := coreType.HostPathCharDev
	deployment := coreAppisType.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-k3s",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"app":             obj.Name + "-k3s",
				"gorizond-deploy": obj.Name,
			},
		},
		Spec: coreAppisType.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": obj.Name + "-k3s",
				},
			},
			Template: coreType.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": obj.Name + "-k3s",
					},
				},
				Spec: coreType.PodSpec{
					AutomountServiceAccountToken: pointer.BoolPtr(false),
					Containers: []coreType.Container{
						{
							Name:            "tailscaled",
							Image:           "tailscale/tailscale",
							ImagePullPolicy: coreType.PullIfNotPresent,
							Args: []string{
								"tailscaled",
							},
							Resources: coreType.ResourceRequirements{
								Requests: coreType.ResourceList{
									coreType.ResourceCPU: resource.MustParse("10m"),
									coreType.ResourceMemory: resource.MustParse("24Mi"),
								},
							},
							SecurityContext: &coreType.SecurityContext{
								RunAsNonRoot:             pointer.BoolPtr(false),
								ReadOnlyRootFilesystem:   pointer.BoolPtr(false),
								Privileged:               pointer.BoolPtr(false),
								AllowPrivilegeEscalation: pointer.BoolPtr(false),
								Capabilities: &coreType.Capabilities{
									Add: []coreType.Capability{
										"NET_ADMIN",
										"NET_RAW",
									},
								},
							},
							VolumeMounts: []coreType.VolumeMount{
								{
									Name:      "dev-tun",
									MountPath: "/dev/net/tun",
								},
								{
									Name:      "tailscale-socket",
									MountPath: "/var/run/tailscale",
								},
								{
									Name:      "tailscale-data",
									MountPath: "/var/lib/tailscale",
								},
								{
									Name:      "var-lib",
									MountPath: "/var/lib",
								},
							},
						},
						{
							Name:            "k3s",
							Image:           "rancher/k3s:" + obj.Spec.KubernetesVersion,
							ImagePullPolicy: coreType.PullIfNotPresent,
							Command: []string{
								"k3s",
								"server",
							},
							Args: []string{
								"--disable-agent",
								"--node-taint=CriticalAddonsOnly=true:NoExecute",
								"--https-listen-port=6443",
								"--tls-san=api-" + domain,
								"--tls-san=" + obj.Name + "-k3s." + obj.Namespace,
								"--tls-san=" + obj.Name + "-k3s." + obj.Namespace + ".svc." + os.Getenv("CLUSTER_DOMAIN_K3S"),
								"--disable=servicelb",
								"--disable=traefik",
								"--disable=local-storage",
								"--disable=metrics-server",
								"--cluster-cidr=10.44.0.0/16",
								"--service-cidr=10.45.0.0/16",
								"--vpn-auth=name=tailscale,joinKey=" + obj.Status.HeadscaleToken + ",controlServerURL=http://headscale-"  + obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE"),
							},
							Resources: coreType.ResourceRequirements{
								Requests: coreType.ResourceList{
									coreType.ResourceCPU: resource.MustParse("100m"),
									coreType.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							Env: []coreType.EnvVar{
								{
									Name:  "PATH",
									Value: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/bin/aux:/tailscale-binary",
								},
								{
									Name:  "K3S_DATASTORE_ENDPOINT",
									Value: strings.ReplaceAll(os.Getenv("DB_DSN_KUBERNETES"), "/gorizond_truncate", "/"+sanitizedNameApi),
								},
								{
									Name:  "K3S_TOKEN",
									Value: obj.Status.HeadscaleToken,
								},
							},
							Ports: []coreType.ContainerPort{
								{
									ContainerPort: 6443,
									Protocol:      coreType.ProtocolTCP,
								},
							},
							VolumeMounts: []coreType.VolumeMount{
								{
									Name:      "k3s-data",
									MountPath: "/var/lib/rancher/k3s/server/",
								},
								{
									Name:      "k3s-yaml",
									MountPath: "/etc/rancher/k3s/",
								},
								{
									Name:      "tailscale-binary",
									MountPath: "/tailscale-binary",
								},
								{
									Name:      "dev-tun",
									MountPath: "/dev/net/tun",
								},
								{
									Name:      "tailscale-socket",
									MountPath: "/var/run/tailscale",
								},
								{
									Name:      "tailscale-data",
									MountPath: "/var/lib/tailscale",
								},
								{
									Name:      "var-lib",
									MountPath: "/var/lib", // TODO check if we need this
								},
								{
									Name:      "k3s-manifest",
									MountPath: "/var/lib/rancher/k3s/server/manifests/cattle-system-manifest.yaml",
									SubPath:   "cattle-system-manifest.yaml",
								},
							},
							LivenessProbe: &coreType.Probe{
								ProbeHandler: coreType.ProbeHandler{
									TCPSocket: &coreType.TCPSocketAction{
										Port: intstr.FromInt(6443),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
							ReadinessProbe: &coreType.Probe{
								ProbeHandler: coreType.ProbeHandler{
									TCPSocket: &coreType.TCPSocketAction{
										Port: intstr.FromInt(6443),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
						},
						{
							Name:  "nginx",
							Image: "nginx:alpine",
							Resources: coreType.ResourceRequirements{
								Requests: coreType.ResourceList{
									coreType.ResourceMemory: resource.MustParse("4Mi"),
								},
							},
							Args: []string{
								`until [ -f /source/token ]; do
	echo "wait /source/token"
	sleep 2
done
until [ -f /yaml/k3s.yaml ]; do
	echo "wait /yaml/k3s.yaml"
	sleep 2
done
cp /source/token /usr/share/nginx/html/token
cp /yaml/k3s.yaml /usr/share/nginx/html/k3s.yaml
sed -i 's|server: https://127.0.0.1:6443|server: https://` + obj.Name + "-k3s." + obj.Namespace + ".svc." + os.Getenv("CLUSTER_DOMAIN_K3S") + `:6443|' /usr/share/nginx/html/k3s.yaml
cp /source/tls/server-ca.crt /usr/share/nginx/html/ca.crt
chmod 777 -R /usr/share/nginx/html
nginx -g 'daemon off;'`,
							},
							Command: []string{
								"sh",
								"-c",
							},
							LivenessProbe: &coreType.Probe{
								ProbeHandler: coreType.ProbeHandler{
									TCPSocket: &coreType.TCPSocketAction{
										Port: intstr.FromInt(80),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
							ReadinessProbe: &coreType.Probe{
								ProbeHandler: coreType.ProbeHandler{
									TCPSocket: &coreType.TCPSocketAction{
										Port: intstr.FromInt(80),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
							ImagePullPolicy: coreType.PullIfNotPresent,
							VolumeMounts: []coreType.VolumeMount{
								{
									Name:      "k3s-data",
									MountPath: "/source",
								},
								{
									Name:      "k3s-yaml",
									MountPath: "/yaml",
								},
							},
						},
					},
					InitContainers: []coreType.Container{
						{
							Name:            "tailscale-binary",
							Image:           "tailscale/tailscale",
							Command:         []string{"sh"},
							Args:            []string{"-c", "cp /usr/local/bin/tailscale /tailscale-binary/tailscale"},
							ImagePullPolicy: coreType.PullIfNotPresent,
							VolumeMounts: []coreType.VolumeMount{
								{
									Name:      "tailscale-binary",
									MountPath: "/tailscale-binary",
								},
							},
						},
					},
					Volumes: []coreType.Volume{
						{
							Name: "dev-tun",
							VolumeSource: coreType.VolumeSource{
								HostPath: &coreType.HostPathVolumeSource{
									Path: "/dev/net/tun",
									Type: &hostPathType,
								},
							},
						},
						{
							Name: "tailscale-binary",
							VolumeSource: coreType.VolumeSource{
								EmptyDir: &coreType.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "tailscale-socket",
							VolumeSource: coreType.VolumeSource{
								EmptyDir: &coreType.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "tailscale-data",
							VolumeSource: coreType.VolumeSource{
								EmptyDir: &coreType.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "headscale-socket",
							VolumeSource: coreType.VolumeSource{
								EmptyDir: &coreType.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "var-lib",
							VolumeSource: coreType.VolumeSource{
								EmptyDir: &coreType.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "k3s-data",
							VolumeSource: coreType.VolumeSource{
								EmptyDir: &coreType.EmptyDirVolumeSource{},
							},
						},
						{
							Name: "k3s-yaml",
							VolumeSource: coreType.VolumeSource{
								EmptyDir: &coreType.EmptyDirVolumeSource{},
							},
						},

						{
							Name: "k3s-manifest",
							VolumeSource: coreType.VolumeSource{
								Secret: &coreType.SecretVolumeSource{
									SecretName: obj.Name + "-manifest",
								},
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

	service := &coreType.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-k3s",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"app":             obj.Name + "-k3s",
				"gorizond-deploy": obj.Name,
			},
		},
		Spec: coreType.ServiceSpec{
			Ports: []coreType.ServicePort{
				{
					Port:       6443,
					Name:       "https",
					Protocol:   coreType.ProtocolTCP,
					TargetPort: intstr.FromInt(6443),
				},
				{
					Port:       80,
					Name:       "http",
					Protocol:   coreType.ProtocolTCP,
					TargetPort: intstr.FromInt(80),
				},
			},
			Selector: map[string]string{
				"app": obj.Name + "-k3s",
			},
		},
	}

	_, err = mgmtCore.Core().V1().Service().Create(service)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Service %s already exists in namespace %s", service.Name, service.Namespace)
		} else {
			return nil, err
		}
	}
	k3s_cert := os.Getenv("GORIZOND_CERT_K3S")
	pathType := "ImplementationSpecific"
	ingress := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-k3s",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"app":             obj.Name + "-k3s",
				"gorizond-deploy": obj.Name,
			},
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/backend-protocol": "HTTPS",
			},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: "api-" + obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster + "." + os.Getenv("GORIZOND_DOMAIN_K3S"),
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: obj.Name + "-k3s",
										Port: networkingv1.ServiceBackendPort{
											Number: 6443,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if k3s_cert != "" {
		ingress.Spec.TLS = []networkingv1.IngressTLS{
			{
				Hosts:      []string{"api-" + obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster + "." + os.Getenv("GORIZOND_DOMAIN_K3S")},
			},
		}
	}

	_, err = NetworkResourceController.Create(&ingress)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Ingress %s already exists in namespace %s", ingress.Name, ingress.Namespace)
		} else {
			return nil, err
		}
	}

	obj.Status.Provisioning = "WaitKubernetesToken"
	return GorizondResourceController.Update(obj)
}

func createKubernetesStorage(sanitizedNameApi string, obj *gorizondv1.Cluster, dbKubernetes *pkg.DatabaseManager, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {
	err := dbKubernetes.CreateDatabase(sanitizedNameApi)
	if err != nil {
		return nil, err
	}
	obj.Status.Provisioning = "WaitKubernetesDeploy"
	return GorizondResourceController.Update(obj)
}

func createHeadScaleToken(ctx context.Context, obj *gorizondv1.Cluster, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {
	gprcTarget := obj.Name + "-headscale." + obj.Namespace + ".svc:50444"

	conn, err := grpc.NewClient(
		gprcTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}

	client := headscalev1.NewHeadscaleServiceClient(conn)

	now := time.Now().UTC()
	futureTime := now.AddDate(100, 0, 0)

	request := &headscalev1.CreatePreAuthKeyRequest{
		User:       obj.Name,
		Reusable:   true,
		Expiration: timestamppb.New(futureTime),
	}
	response, err := client.CreatePreAuthKey(ctx, request)
	if err != nil {
		return nil, err
	}
	obj.Status.HeadscaleToken = response.GetPreAuthKey().GetKey()
	obj.Status.Provisioning = "WaitKubernetesStorage"
	return GorizondResourceController.Update(obj)
}

func createHeadScaleCreateUser(ctx context.Context, obj *gorizondv1.Cluster, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {

	err := WaitFor404(obj.Name+"-headscale."+obj.Namespace+".svc." + os.Getenv("CLUSTER_DOMAIN_HEADSCALE") + ":8080", ctx)
	if err != nil {
		return nil, err
	}

	gprcTarget := obj.Name + "-headscale." + obj.Namespace + ".svc." + os.Getenv("CLUSTER_DOMAIN_HEADSCALE") + ":50444"
	conn, err := grpc.NewClient(
		gprcTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}

	client := headscalev1.NewHeadscaleServiceClient(conn)
	_, err = client.CreateUser(ctx, &headscalev1.CreateUserRequest{Name: obj.Name})
	if err != nil {
		// TODO check user exist (loop running)
		return nil, err
	}
	obj.Status.Provisioning = "WaitHeadScaleToken"
	return GorizondResourceController.Update(obj)
}

func WaitFor404(address string, ctx context.Context) error {

	client := &http.Client{
		Timeout: 2 * time.Second,
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("Wait headscale %s not ready", address)
		case <-ticker.C:
			resp, err := client.Get("http://" + address)
			if err != nil {
				continue
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound {
				return nil
			}
		}
	}
}

func createHeadScaleCreate(obj *gorizondv1.Cluster, mgmtCore *core.Factory, mgmtApps *apps.Factory, NetworkResourceController controllersIngressv1.IngressController, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {
	deployment := coreAppisType.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-headscale",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"app":             obj.Name + "-headscale",
				"gorizond-deploy": obj.Name,
			},
		},
		Spec: coreAppisType.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": obj.Name + "-headscale",
				},
			},
			Template: coreType.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": obj.Name + "-headscale",
					},
				},
				Spec: coreType.PodSpec{
					Containers: []coreType.Container{
						{

							Name:            "headscale",
							Image:           "headscale/headscale:stable-debug",
							ImagePullPolicy: coreType.PullIfNotPresent,
							Args: []string{
								"serve",
							},
							Resources: coreType.ResourceRequirements{
								Requests: coreType.ResourceList{
									coreType.ResourceCPU: resource.MustParse("10m"),
									coreType.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
							Ports: []coreType.ContainerPort{
								{
									ContainerPort: 8080,
									Protocol:      coreType.ProtocolTCP,
								},
							},
							VolumeMounts: []coreType.VolumeMount{
								{
									Name:      "headscale-config",
									MountPath: "/etc/headscale",
								},
								{
									Name:      "socket-file",
									MountPath: "/var/run/headscale",
								},
							},
							LivenessProbe: &coreType.Probe{
								ProbeHandler: coreType.ProbeHandler{
									TCPSocket: &coreType.TCPSocketAction{
										Port: intstr.FromInt(8080),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
						},
						{
							Name:            "socat",
							Image:           "alpine/socat",
							ImagePullPolicy: coreType.PullIfNotPresent,
							Args: []string{
								"TCP-LISTEN:50444,fork",
								"UNIX-CONNECT:/var/run/headscale/headscale.sock",
							},
							Resources: coreType.ResourceRequirements{
								Requests: coreType.ResourceList{
									coreType.ResourceMemory: resource.MustParse("4Mi"),
								},
							},
							Ports: []coreType.ContainerPort{
								{
									ContainerPort: 50444,
									Protocol:      coreType.ProtocolTCP,
								},
							},
							VolumeMounts: []coreType.VolumeMount{
								{
									Name:      "socket-file",
									MountPath: "/var/run/headscale",
								},
							},
							LivenessProbe: &coreType.Probe{
								ProbeHandler: coreType.ProbeHandler{
									TCPSocket: &coreType.TCPSocketAction{
										Port: intstr.FromInt(50444),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
							},
						},
					},
					Volumes: []coreType.Volume{
						{
							Name: "headscale-config",
							VolumeSource: coreType.VolumeSource{
								Secret: &coreType.SecretVolumeSource{
									SecretName: obj.Name + "-headscale",
								},
							},
						},
						{
							Name: "socket-file",
							VolumeSource: coreType.VolumeSource{
								EmptyDir: &coreType.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	_, err := mgmtApps.Apps().V1().Deployment().Create(&deployment)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Deployment %s already exists in namespace %s", deployment.Name, deployment.Namespace)
		} else {
			return nil, err
		}
	}

	service := &coreType.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-headscale",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"app":             obj.Name + "-headscale",
				"gorizond-deploy": obj.Name,
			},
		},
		Spec: coreType.ServiceSpec{
			Ports: []coreType.ServicePort{
				{
					Port:       8080,
					Name:       "http",
					Protocol:   coreType.ProtocolTCP,
					TargetPort: intstr.FromInt(8080),
				},
				{
					Port:       50444,
					Name:       "grpc",
					Protocol:   coreType.ProtocolTCP,
					TargetPort: intstr.FromInt(50444),
				},
			},
			Selector: map[string]string{
				"app": obj.Name + "-headscale",
			},
		},
	}

	_, err = mgmtCore.Core().V1().Service().Create(service)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Service %s already exists in namespace %s", service.Name, service.Namespace)
		} else {
			return nil, err
		}
	}
	
	hs_cert := os.Getenv("GORIZOND_CERT_HEADSCALE")
	pathType := "ImplementationSpecific"
	ingress := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-headscale",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"app":             obj.Name + "-headscale",
				"gorizond-deploy": obj.Name,
			},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: "headscale-" + obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE"),
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: obj.Name + "-headscale",
										Port: networkingv1.ServiceBackendPort{
											Number: 8080,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if hs_cert != "" {
		ingress.Spec.TLS = []networkingv1.IngressTLS{
			{
				Hosts:      []string{"headscale-" + obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE")},
			},
		}
	}

	_, err = NetworkResourceController.Create(&ingress)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Ingress %s already exists in namespace %s", ingress.Name, ingress.Namespace)
		} else {
			return nil, err
		}
	}
	obj.Status.Provisioning = "WaitHeadScaleCreateUser"
	return GorizondResourceController.Update(obj)
}

func createHeadScaleConfig(FullSanitizedName string, obj *gorizondv1.Cluster, dsnHeadScale *dsnparser.DSN, SecretResourceController corev1.SecretController, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {
	machineKey := key.NewMachine()
	machineKeyStr, err := machineKey.MarshalText()
	if err != nil {
		return nil, err
	}
	domain := obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE")
	// Create the Secret object
	secret := &coreType.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-headscale",
			Namespace: obj.Namespace,
			Labels: map[string]string{
				"gorizond-deploy": obj.Name,
			},
		},
		Data: map[string][]byte{
			"noise_private.key": machineKeyStr,
			"config.yaml": []byte(fmt.Sprintf(`server_url: "http://headscale-%s"
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 0.0.0.0:9090
grpc_listen_addr: 0.0.0.0:50443
grpc_allow_insecure: true
noise:
  private_key_path: /etc/headscale/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  allocation: sequential
derp:
  server:
    enabled: false
  urls:
    - https://controlplane.tailscale.com/derpmap/default
  paths: []
  auto_update_enabled: true
  update_frequency: 24h
disable_check_updates: true
ephemeral_node_inactivity_timeout: 30m
database:
  type: postgres
  debug: false
  gorm:
    prepare_stmt: true
    parameterized_queries: true
    skip_err_record_not_found: true
    slow_threshold: 1000
  postgres:
    host: "%s"
    port: "%s"
    name: "%s"
    user: "%s"
    pass: "%s"
    max_open_conns: 10
    max_idle_conns: 10
    conn_max_idle_time_secs: 3600
    ssl: false
log:
  format: text
  level: info
dns:
  magic_dns: true
  base_domain: gotizond
  nameservers:
    global:
      - 10.43.0.10 #TODO local k8s
      - 1.1.1.1
      - 1.0.0.1
      - 2606:4700:4700::1111
      - 2606:4700:4700::1001
    split:
      {}
  search_domains: []
  extra_records: []
unix_socket: /var/run/headscale/headscale.sock
unix_socket_permission: "0770"
logtail:
  enabled: false
randomize_client_port: false
`, domain, dsnHeadScale.GetHost(), dsnHeadScale.GetPort(), FullSanitizedName, dsnHeadScale.GetUser(), dsnHeadScale.GetPassword())),
		},
	}
	// Create the Secret in Kubernetes
	_, err = SecretResourceController.Create(secret)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Secret %s already exists in namespace %s", secret.Name, secret.Namespace)
		} else {
			return nil, err
		}
	}
	obj.Status.Provisioning = "WaitHeadScaleCreate"
	return GorizondResourceController.Update(obj)
}

func createHeadScaleDatabase(sanitizedNameHs string, obj *gorizondv1.Cluster, dbHeadScale *pkg.DatabaseManager, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {
	log.Infof("Creating headscale database %s", sanitizedNameHs)
	err := dbHeadScale.CreateDatabase(sanitizedNameHs)
	if err != nil {
		return nil, err
	}
	obj.Status.Provisioning = "WaitHeadScaleConfig"
	return GorizondResourceController.Update(obj)
}

func createCattleCluster(obj *gorizondv1.Cluster, mgmt *controllersProvision.Factory, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {
	newCluster := &cattlev1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name,
			Namespace: obj.Namespace,
			Annotations: map[string]string{
				"field.cattle.io/no-creator-rbac": "true",
			},
		},
		//Spec: cattlev1.ClusterSpec{
		//	ClusterAPIConfig: &cattlev1.ClusterAPIConfig{
		//		ClusterName: obj.Name,
		//	},
		//},
	}
	newCluster, err := mgmt.Provisioning().V1().Cluster().Create(newCluster)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Cattle Cluster %s already exists in namespace %s", newCluster.Name, newCluster.Namespace)
		} else {
			return nil, err
		}
	}
	log.Infof("Successfully created: %s.%s", newCluster.Namespace, newCluster.Name)
	// obj = obj.DeepCopy()
	obj.Status.Provisioning = "WaitHeadScaleDatabase"
	return GorizondResourceController.Update(obj)
}
