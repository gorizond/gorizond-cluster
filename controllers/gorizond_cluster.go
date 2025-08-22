package controllers

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorizond/gorizond-cluster/pkg"
	gorizondv1 "github.com/gorizond/gorizond-cluster/pkg/apis/provisioning.gorizond.io/v1"
	controllers "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io"
	controllersv1 "github.com/gorizond/gorizond-cluster/pkg/generated/controllers/provisioning.gorizond.io/v1"
	"github.com/kos-v/dsnparser"
	fleetTypev1alpha1 "github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	"github.com/rancher/lasso/pkg/log"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	cattlev1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	controllersFleet "github.com/rancher/rancher/pkg/generated/controllers/fleet.cattle.io"
	controllersFleetv1alpha1 "github.com/rancher/rancher/pkg/generated/controllers/fleet.cattle.io/v1alpha1"
	controllersManagement "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io"
	controllersManagementv3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	controllersProvision "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io"
	controllersProvisionv1 "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io/v1"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	corev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/v3/pkg/kubeconfig"
	batchv1 "k8s.io/api/batch/v1"
	coreType "k8s.io/api/core/v1"
	errorsk8s "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
)

const k3sHelmChart = `
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: %s-k3s
  namespace: %s
spec:
  chart: k3s
  repo: https://gorizond.github.io/fleet-gorizond-charts/
  version: %s
  timeout: 15m0s
  set:
    image.k3s.tag: %s
    database: %s
    token: %s
    headscaleControlServerURL: %s
    ingress.hosts[0].host: %s
    ingress.hosts[0].paths[0].path: /
    ingress.hosts[0].paths[0].pathType: ImplementationSpecific
`

const headscaleHelmChart = `
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: %s-headscale
  namespace: %s
spec:
  chart: headscale
  repo: https://gorizond.github.io/fleet-gorizond-charts/
  version: %s
  timeout: 15m0s
  set:
    database.host: %s
    database.name: %s
    database.pass: %s
    database.port: %s
    database.user: %s
    ingress.hosts[0].host: %s
    ingress.hosts[0].paths[0].path: /
    ingress.hosts[0].paths[0].pathType: ImplementationSpecific
    noise_private: %s
`

const k3sHelmChartVersion = "0.1.3"
const headscaleHelmChartVersion = "0.1.14"

func getK3sChartVersion() string {
	if v := os.Getenv("K3S_HELM_CHART_VERSION"); v != "" {
		return v
	}
	return k3sHelmChartVersion
}

func getHeadscaleChartVersion() string {
	if v := os.Getenv("HEADSCALE_HELM_CHART_VERSION"); v != "" {
		return v
	}
	return headscaleHelmChartVersion
}

func InitClusterController(ctx context.Context, mgmtGorizond *controllers.Factory, mgmtManagement *controllersManagement.Factory, mgmtProvision *controllersProvision.Factory, mgmtCore *core.Factory, mgmtFleet *controllersFleet.Factory) {
	GorizondResourceController := mgmtGorizond.Provisioning().V1().Cluster()
	SecretResourceController := mgmtCore.Core().V1().Secret()
	NamespaceResourceController := mgmtCore.Core().V1().Namespace()
	ProvisionResourceController := mgmtProvision.Provisioning().V1().Cluster()
	ClusterRoleTemplateBindingController := mgmtManagement.Management().V3().ClusterRoleTemplateBinding()
	UserController := mgmtManagement.Management().V3().User()
	FleetWorkspaceController := mgmtManagement.Management().V3().FleetWorkspace()
	FleetBundleController := mgmtFleet.Fleet().V1alpha1().Bundle()
	RegistrationTokenResourceController := mgmtManagement.Management().V3().ClusterRegistrationToken()
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
	GorizondResourceController.OnChange(ctx, "k3s-version", func(key string, obj *gorizondv1.Cluster) (*gorizondv1.Cluster, error) {
		if obj == nil {
			return nil, nil
		}
		if obj.Status.Provisioning != "Done" {
			return nil, nil
		}
		if obj.Spec.KubernetesVersion != obj.Status.K3sVersion {
			// upgrade k3s deployment
			uniqName := obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster
			k3sDomain := "api-" + uniqName + "." + os.Getenv("GORIZOND_DOMAIN_K3S")
			HSServerURL := "http://headscale-" + uniqName + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE")
			sanitizedNameApi := strings.NewReplacer(
				".", "_",
				"-", "_",
				" ", "_",
				"@", "_",
				"#", "_",
				"$", "_",
			).Replace(obj.Namespace + "_api_" + obj.Status.Cluster)
			fleetBundle, err := FleetBundleController.Get("fleet-default", uniqName, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			yamlContent := fmt.Sprintf(k3sHelmChart,
				obj.Name,
				obj.Namespace,
				getK3sChartVersion(),
				obj.Spec.KubernetesVersion,
				strings.ReplaceAll(os.Getenv("DB_DSN_KUBERNETES"), "/gorizond_truncate", "/"+sanitizedNameApi),
				obj.Status.HeadscaleToken,
				HSServerURL,
				k3sDomain)
			encodedContent, err := compressAndEncode(yamlContent)
			if err != nil {
				return nil, err
			}
			// Update the existing helm-k3s.yaml resource or add it if it does not exist,
			// without affecting other resources (for example, helm-headscale.yaml)
			updated := false
			for i := range fleetBundle.Spec.Resources {
				if fleetBundle.Spec.Resources[i].Name == "helm-k3s.yaml" {
					fleetBundle.Spec.Resources[i].Content = encodedContent
					fleetBundle.Spec.Resources[i].Encoding = "base64+gz"
					updated = true
					break
				}
			}
			if !updated {
				fleetBundle.Spec.Resources = append(fleetBundle.Spec.Resources, fleetTypev1alpha1.BundleResource{
					Content:  encodedContent,
					Name:     "helm-k3s.yaml",
					Encoding: "base64+gz",
				})
			}
			if _, err := FleetBundleController.Update(fleetBundle); err != nil {
				return nil, err
			}
			log.Infof("Updated k3s version to %s-->%s for cluster %s/%s", obj.Status.K3sVersion, obj.Spec.KubernetesVersion, obj.Namespace, obj.Name)
			obj.Status.K3sVersion = obj.Spec.KubernetesVersion
			return GorizondResourceController.Update(obj)
		}
		return obj, nil
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

			if obj.Status.Provisioning == "WaitAddAdminMember" {
				return AddAdminMember(obj, UserController, FleetWorkspaceController, GorizondResourceController, ClusterRoleTemplateBindingController)
			}

			if obj.Status.HeadscaleToken == "" && obj.Status.Provisioning == "WaitHeadScaleCreate" {
				return createHeadScaleCreate(obj, FleetBundleController, GorizondResourceController, sanitizedNameHs)
			}

			if obj.Status.HeadscaleToken == "" && obj.Status.Provisioning == "WaitHeadScaleToken" {
				return createHeadScaleToken(obj, ctx, SecretResourceController, ProvisionResourceController, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken != "" && obj.Status.K3sToken == "" && obj.Status.Provisioning == "WaitKubernetesCreate" {
				return createKubernetesCreate(obj, FleetBundleController, GorizondResourceController, sanitizedNameApi)
			}
			if obj.Status.HeadscaleToken != "" && obj.Status.K3sToken == "" && obj.Status.Provisioning == "WaitKubernetesToken" {
				return createKubernetesToken(obj, ctx, SecretResourceController, ProvisionResourceController, GorizondResourceController, RegistrationTokenResourceController)
			}
		}
		return obj, nil
	})
	GorizondResourceController.OnRemove(ctx, "gorizond-cleanup", func(key string, obj *gorizondv1.Cluster) (*gorizondv1.Cluster, error) {
		selector := fmt.Sprintf("gorizond-deploy=%s,workspace=%s", obj.Name, obj.Namespace)
		clusters, err := mgmtProvision.Provisioning().V1().Cluster().List(obj.Namespace, metav1.ListOptions{})
		if err != nil {
			return obj, nil
		}
		for _, cluster := range clusters.Items {
			// list all Clusters in workspace and remove if gorizond.Name == cluster.Name
			if cluster.Name == obj.Name {
				err = mgmtProvision.Provisioning().V1().Cluster().Delete(obj.Namespace, cluster.Name, nil)
				if err != nil {
					return obj, nil
				}
				log.Infof("deleted cluster %s (%s)", cluster.Name, obj.Namespace)
			}
		}

		bundle, err := FleetBundleController.List("fleet-default", metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return obj, nil
		}
		for _, service := range bundle.Items {
			err = FleetBundleController.Delete("fleet-default", service.Name, nil)
			if err != nil {
				return obj, nil
			}
			log.Infof("deleted bundle %s (%s)", service.Name, "fleet-default")
		}
		log.Infof("Successfully remove: %s.%s", obj.Namespace, obj.Name)

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

		runtimeClusters, err := ProvisionResourceController.List("fleet-default", metav1.ListOptions{LabelSelector: "gorizond.runtime=true"})
		if err != nil {
			return nil, err
		}

		for _, cluster := range runtimeClusters.Items {
			secret, err := SecretResourceController.Get("fleet-default", cluster.Name+"-kubeconfig", metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			// create k8s client for gorizond runtime cluster
			var clientConfig *rest.Config
			if os.Getenv("DEBUG_LOCAL_KUBECOFING") != "" {
				clientConfig, err = kubeconfig.GetNonInteractiveClientConfig(os.Getenv("DEBUG_LOCAL_KUBECOFING")).ClientConfig()
			} else {
				clientConfig, err = pkg.GetRestConfig(secret.Data["value"])
			}
			if err != nil {
				return nil, err
			}
			clientset, err := pkg.CreateClientset(clientConfig)
			if err != nil {
				return nil, err
			}

			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "drop-db-headscale-" + obj.Status.Cluster,
					Namespace: "default",
				},
				Spec: batchv1.JobSpec{
					TTLSecondsAfterFinished: pointer.Int32(20), // Job will be deleted 100 seconds after it
					Template: coreType.PodTemplateSpec{
						Spec: coreType.PodSpec{
							Containers: []coreType.Container{
								{
									Name:            "drop-db",
									Image:           imageFromDsn(os.Getenv("DB_DSN_HEADSCALE")),
									ImagePullPolicy: coreType.PullIfNotPresent,
									Command: []string{
										"/bin/sh",
										"-c",
									},
									Args: ArgsFromDsn(os.Getenv("DB_DSN_HEADSCALE"), sanitizedNameHs),
								},
							},
							RestartPolicy: coreType.RestartPolicyNever,
						},
					},
				},
			}
			_, err = clientset.BatchV1().Jobs("default").Create(ctx, job, metav1.CreateOptions{})
			if err != nil {
				return nil, err
			}

			job = &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "drop-db-k3s-" + obj.Status.Cluster,
					Namespace: "default",
				},
				Spec: batchv1.JobSpec{
					TTLSecondsAfterFinished: pointer.Int32(20), // Job will be deleted 100 seconds after it
					Template: coreType.PodTemplateSpec{
						Spec: coreType.PodSpec{
							Containers: []coreType.Container{
								{
									Name:            "drop-db",
									Image:           imageFromDsn(os.Getenv("DB_DSN_KUBERNETES")),
									ImagePullPolicy: coreType.PullIfNotPresent,
									Command: []string{
										"/bin/sh",
										"-c",
									},
									Args: ArgsFromDsn(os.Getenv("DB_DSN_KUBERNETES"), sanitizedNameApi),
								},
							},
							RestartPolicy: coreType.RestartPolicyNever,
						},
					},
				},
			}
			_, err = clientset.BatchV1().Jobs("default").Create(ctx, job, metav1.CreateOptions{})
			if err != nil {
				return nil, err
			}
		}
		return obj, nil
	})
}

func ArgsFromDsn(dns string, database string) []string {
	if strings.HasPrefix(dns, "postgres://") {
		return []string{
			"psql " + dns + " -c \"INSERT INTO table_to_deletes (name) VALUES ('" + database + "');\"",
		}
	}
	if strings.HasPrefix(dns, "mysql://") {
		dnsParse := dsnparser.Parse(dns)
		return []string{
			"mysql --user=" + dnsParse.GetUser() + " --host=" + dnsParse.GetHost() + " --port=" + dnsParse.GetPort() + " --password=" + dnsParse.GetPassword() + " --database=" + dnsParse.GetPath() + " -e \"INSERT INTO " + dnsParse.GetPath() + ".table_to_deletes (name) VALUES ('" + database + "');\"",
		}
	}
	return []string{"ls"}
}

func imageFromDsn(dns string) string {
	if strings.HasPrefix(dns, "postgres://") {
		return "postgres:alpine"
	}
	if strings.HasPrefix(dns, "mysql://") {
		return "mysql"
	}
	return "busybox"
}

func createKubernetesToken(obj *gorizondv1.Cluster, ctx context.Context, SecretResourceController corev1.SecretController, ProvisionResourceController controllersProvisionv1.ClusterController, GorizondResourceController controllersv1.ClusterController, RegistrationTokenResourceController controllersManagementv3.ClusterRegistrationTokenController) (*gorizondv1.Cluster, error) {
	clusters, err := ProvisionResourceController.List("fleet-default", metav1.ListOptions{LabelSelector: "gorizond.runtime=true"})
	if err != nil {
		return nil, err
	}
	for _, cluster := range clusters.Items {
		secret, err := SecretResourceController.Get("fleet-default", cluster.Name+"-kubeconfig", metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		// create k8s client for gorizond runtime cluster
		var clientConfig *rest.Config
		if os.Getenv("DEBUG_LOCAL_KUBECOFING") != "" {
			clientConfig, err = kubeconfig.GetNonInteractiveClientConfig(os.Getenv("DEBUG_LOCAL_KUBECOFING")).ClientConfig()
		} else {
			clientConfig, err = pkg.GetRestConfig(secret.Data["value"])
		}
		if err != nil {
			return nil, err
		}
		// parameters headscalek3s
		namespace := obj.Status.Namespace
		deploymentName := obj.Name + "-k3s"
		containerName := "k3s"

		clientset, err := pkg.CreateClientset(clientConfig)
		if err != nil {
			return nil, err
		}

		podName, err, wait := pkg.GetPodName(clientset, ctx, namespace, deploymentName, containerName)
		if wait {
			time.Sleep(5 * time.Second)
			obj.Status.LastTransitionTime = metav1.NewTime(time.Now().UTC())
			return GorizondResourceController.Update(obj)
		}

		if err != nil {
			return nil, err
		}

		command := []string{"cat", "/var/lib/rancher/k3s/server/token"}
		stdout, _, err := pkg.ExecCommand(clientConfig, clientset, namespace, podName, containerName, command)
		if err != nil {
			return nil, err
		}

		if stdout == "" {
			time.Sleep(5 * time.Second)
			obj.Status.LastTransitionTime = metav1.NewTime(time.Now().UTC())
			return GorizondResourceController.Update(obj)
		}

		// Add rancher registration to k3s
		registration, err := RegistrationTokenResourceController.Get(obj.Status.Cluster, "default-token", metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		command = strings.Split(registration.Status.Command, " ")
		_, _, err = pkg.ExecCommand(clientConfig, clientset, namespace, podName, containerName, command)
		if err != nil {
			return nil, err
		}

		obj.Status.K3sToken = removeEmptyLines(stdout)
		obj.Status.Provisioning = "Done"
		break
	}

	return GorizondResourceController.Update(obj)
}

func createKubernetesCreate(obj *gorizondv1.Cluster, FleetBundleController controllersFleetv1alpha1.BundleController, GorizondResourceController controllersv1.ClusterController, sanitizedNameApi string) (*gorizondv1.Cluster, error) {
	uniqName := obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster
	k3sDomain := "api-" + uniqName + "." + os.Getenv("GORIZOND_DOMAIN_K3S")
	HSServerURL := "http://headscale-" + uniqName + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE")
	fleetBundle, err := FleetBundleController.Get("fleet-default", uniqName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	yamlContent := fmt.Sprintf(k3sHelmChart,
		obj.Name,
		obj.Namespace,
		getK3sChartVersion(),
		obj.Spec.KubernetesVersion,
		strings.ReplaceAll(os.Getenv("DB_DSN_KUBERNETES"), "/gorizond_truncate", "/"+sanitizedNameApi),
		obj.Status.HeadscaleToken,
		HSServerURL,
		k3sDomain)
	encodedContent, err := compressAndEncode(yamlContent)
	if err != nil {
		return nil, err
	}
	fleetBundle.Spec.Resources = append(fleetBundle.Spec.Resources, fleetTypev1alpha1.BundleResource{
		Content:  encodedContent,
		Name:     "helm-k3s.yaml",
		Encoding: "base64+gz",
	})
	FleetBundleController.Update(fleetBundle)
	obj.Status.Provisioning = "WaitKubernetesToken"
	obj.Status.K3sVersion = obj.Spec.KubernetesVersion
	return GorizondResourceController.Update(obj)
}

func createHeadScaleToken(obj *gorizondv1.Cluster, ctx context.Context, SecretResourceController corev1.SecretController, ProvisionResourceController controllersProvisionv1.ClusterController, GorizondResourceController controllersv1.ClusterController) (*gorizondv1.Cluster, error) {
	clusters, err := ProvisionResourceController.List("fleet-default", metav1.ListOptions{LabelSelector: "gorizond.runtime=true"})
	if err != nil {
		return nil, err
	}
	for _, cluster := range clusters.Items {
		secret, err := SecretResourceController.Get("fleet-default", cluster.Name+"-kubeconfig", metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		// create k8s client for gorizond runtime cluster
		var clientConfig *rest.Config
		if os.Getenv("DEBUG_LOCAL_KUBECOFING") != "" {
			clientConfig, err = kubeconfig.GetNonInteractiveClientConfig(os.Getenv("DEBUG_LOCAL_KUBECOFING")).ClientConfig()
		} else {
			clientConfig, err = pkg.GetRestConfig(secret.Data["value"])
		}
		if err != nil {
			return nil, err
		}
		// parameters headscale
		namespace := obj.Status.Namespace
		deploymentName := obj.Name + "-headscale"
		containerName := "headscale"

		clientset, err := pkg.CreateClientset(clientConfig)
		if err != nil {
			return nil, err
		}

		podName, err, wait := pkg.GetPodName(clientset, ctx, namespace, deploymentName, containerName)
		if wait {
			time.Sleep(5 * time.Second)
			obj.Status.LastTransitionTime = metav1.NewTime(time.Now().UTC())
			return GorizondResourceController.Update(obj)
		}
		if err != nil {
			return nil, err
		}

		command := []string{"headscale", "configtest"}
		_, _, err = pkg.ExecCommand(clientConfig, clientset, namespace, podName, containerName, command)
		if err != nil {
			return nil, err
		}

		command = []string{"headscale", "users", "create", "gorizond"}
		_, _, err = pkg.ExecCommand(clientConfig, clientset, namespace, podName, containerName, command)
		if err != nil {
			// Check if the error is related to exit code 1 (user already exists)
			// or another error, try to check the existence of the user via headscale users list
			log.Infof("Error while creating user gorizond: %v", err)
			commandCheck := []string{"headscale", "users", "list"}
			stdoutCheck, _, errCheck := pkg.ExecCommand(clientConfig, clientset, namespace, podName, containerName, commandCheck)
			if errCheck != nil {
				return nil, fmt.Errorf("error while checking existence of user gorizond: %v", errCheck)
			}
			// Check if the user gorizond is present in the list
			if !strings.Contains(stdoutCheck, "gorizond") {
				return nil, fmt.Errorf("user gorizond not found after creation attempt: %v", err)
			}
			// If the user is found, continue execution
		}

		command = []string{"headscale", "preauthkeys", "create", "--user", "1", "--reusable", "--expiration", "100y"}
		stdout, _, err := pkg.ExecCommand(clientConfig, clientset, namespace, podName, containerName, command)
		if err != nil {
			return nil, err
		}

		obj.Status.HeadscaleToken = removeEmptyLines(stdout)
		obj.Status.Provisioning = "WaitKubernetesCreate"
		break
	}

	return GorizondResourceController.Update(obj)
}

func removeEmptyLines(s string) string {
	lines := strings.Split(s, "\n")
	var nonEmptyLines []string
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmptyLines = append(nonEmptyLines, line)
		}
	}
	return strings.Join(nonEmptyLines, "\n")
}

func generateNoisePrivateHex() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func createHeadScaleCreate(obj *gorizondv1.Cluster, FleetBundleController controllersFleetv1alpha1.BundleController, GorizondResourceController controllersv1.ClusterController, sanitizedNameHs string) (*gorizondv1.Cluster, error) {
	noisePrivateHex, err := generateNoisePrivateHex()
	if err != nil {
		return nil, err
	}
	domain := "headscale-" + obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE")

	dsnHeadScale := dsnparser.Parse(os.Getenv("DB_DSN_HEADSCALE"))
	yamlContent := fmt.Sprintf(headscaleHelmChart,
		obj.Name,
		obj.Namespace,
		getHeadscaleChartVersion(),
		dsnHeadScale.GetHost(),
		sanitizedNameHs,
		dsnHeadScale.GetPassword(),
		dsnHeadScale.GetPort(),
		dsnHeadScale.GetUser(),
		domain, "privkey:"+noisePrivateHex)
	encodedContent, err := compressAndEncode(yamlContent)
	if err != nil {
		return nil, err
	}
	bundle := &fleetTypev1alpha1.Bundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster,
			Namespace: "fleet-default",
			Labels: map[string]string{
				"gorizond-deploy": obj.Name,
				"workspace":       obj.Namespace,
			},
		},
		Spec: fleetTypev1alpha1.BundleSpec{
			BundleDeploymentOptions: fleetTypev1alpha1.BundleDeploymentOptions{
				IgnoreOptions: fleetTypev1alpha1.IgnoreOptions{
					Conditions: []map[string]string{
						{
							"status": "False",
							"type":   "Failed",
						},
					},
				},
			},
			Targets: []fleetTypev1alpha1.BundleTarget{
				fleetTypev1alpha1.BundleTarget{
					ClusterSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"gorizond.runtime": "true",
						},
					},
				},
			},
			TargetRestrictions: []fleetTypev1alpha1.BundleTargetRestriction{
				fleetTypev1alpha1.BundleTargetRestriction{
					ClusterSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"gorizond.runtime": "true",
						},
					},
				},
			},
			Resources: []fleetTypev1alpha1.BundleResource{
				fleetTypev1alpha1.BundleResource{
					Content:  encodedContent,
					Name:     "helm-headscale.yaml",
					Encoding: "base64+gz",
				},
			},
		},
	}

	_, err = FleetBundleController.Create(bundle)
	if err != nil {
		if errorsk8s.IsAlreadyExists(err) {
			log.Infof("Bundle %s already exists in namespace %s", bundle.Name, bundle.Namespace)
		} else {
			return nil, err
		}
	}

	obj.Status.Provisioning = "WaitHeadScaleToken"
	return GorizondResourceController.Update(obj)
}

func compressAndEncode(content string) (string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(content)); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func AddAdminMember(obj *gorizondv1.Cluster, UserController controllersManagementv3.UserController, FleetWorkspaceController controllersManagementv3.FleetWorkspaceController, GorizondResourceController controllersv1.ClusterController, ClusterRoleTemplateBindingController controllersManagementv3.ClusterRoleTemplateBindingController) (*gorizondv1.Cluster, error) {
	fleetworkspace, err := FleetWorkspaceController.Get(obj.Namespace, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	for k := range fleetworkspace.Annotations {
		if strings.HasPrefix(k, "gorizond-user.") && strings.HasSuffix(k, ".admin") {
			parts := strings.SplitN(k[len("gorizond-user."):], ".", 2)
			userID := parts[0]
			user, err := UserController.Get(userID, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			var principalID string
			for _, id := range user.PrincipalIDs {
				if !strings.HasPrefix(id, "local://") {
					principalID = id
					break
				} else {

				}
			}
			if principalID == "" {
				// use local
				principalID = "local://" + user.Name
			}
			newRTB := &v3.ClusterRoleTemplateBinding{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "gorizond-crtb-",
					Namespace:    obj.Status.Cluster,
				},
				ClusterName:       obj.Status.Cluster,
				RoleTemplateName:  "cluster-owner",
				UserName:          userID,
				UserPrincipalName: principalID,
			}
			_, err = ClusterRoleTemplateBindingController.Create(newRTB)
			if err != nil {
				return nil, err
			}
		}
	}
	obj.Status.Provisioning = "WaitHeadScaleCreate"
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
	obj.Status.Provisioning = "WaitAddAdminMember"
	return GorizondResourceController.Update(obj)
}
