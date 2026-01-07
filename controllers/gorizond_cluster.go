package controllers

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
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
	"github.com/rancher/lasso/pkg/log"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	cattlev1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"
)

var helmOpGVR = schema.GroupVersionResource{
	Group:    "fleet.cattle.io",
	Version:  "v1alpha1",
	Resource: "helmops",
}

const k3sHelmChartVersion = "0.1.3"
const headscaleHelmChartVersion = "0.1.15"

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

func normalizeDNSLabelWithMax(base, hashSource string, maxLen int) string {
	name := strings.ToLower(strings.TrimSpace(base))
	if name == "" {
		name = "g"
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	name = strings.Trim(b.String(), "-")
	if name == "" {
		name = "g"
	}

	sum := sha1.Sum([]byte(hashSource))
	hash := hex.EncodeToString(sum[:])[:8]

	suffixLen := len(hash) + 1
	if len(name) > maxLen-suffixLen {
		name = strings.Trim(name[:maxLen-suffixLen], "-")
		if name == "" {
			name = "g"
		}
	}

	return name + "-" + hash
}

func normalizeDNSLabel(base, hashSource string) string {
	return normalizeDNSLabelWithMax(base, hashSource, 63)
}

func normalizeHelmReleaseName(base, hashSource string) string {
	return normalizeDNSLabelWithMax(base, hashSource, 53)
}

func ensureDNSLabel(base, hashSource string) string {
	label := strings.ToLower(strings.TrimSpace(base))
	label = strings.Trim(label, "-")
	if label == "" {
		label = "g"
	}
	if len(label) <= 63 {
		return label
	}
	return normalizeDNSLabel(label, hashSource)
}

func validateProvisionClusterName(name string) error {
	if len(name) < 2 || len(name) > 63 {
		return fmt.Errorf("cluster name must be between 2 and 63 characters")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("cluster name must not begin or end with a hyphen")
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return fmt.Errorf("cluster name can only contain lowercase alphanumeric characters or '-'")
	}
	if name == "local" {
		return fmt.Errorf("cluster name cannot be \"local\"")
	}
	if len(name) == 7 && name[0] == 'c' && name[1] == '-' {
		valid := true
		for i := 2; i < 7; i++ {
			ch := name[i]
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
				continue
			}
			valid = false
			break
		}
		if valid {
			return fmt.Errorf("cluster name cannot be of the form \"c-xxxxx\"")
		}
	}
	return nil
}

func gorizondUniqLabel(obj *gorizondv1.Cluster, prefix string) string {
	base := prefix + obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster
	return normalizeDNSLabel(base, obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/"+prefix)
}

func buildHelmOp(name, namespace, targetNamespace string, labels map[string]string, helm map[string]interface{}) *unstructured.Unstructured {
	if labels == nil {
		labels = map[string]string{}
	}
	spec := map[string]interface{}{
		"helm":             helm,
		"namespace":        targetNamespace,
		"defaultNamespace": targetNamespace,
		"correctDrift": map[string]interface{}{
			"enabled": true,
		},
		"targets": []interface{}{
			map[string]interface{}{
				"clusterSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"gorizond.runtime": "true",
					},
				},
			},
		},
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "fleet.cattle.io/v1alpha1",
			"kind":       "HelmOp",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"labels":    labels,
			},
			"spec": spec,
		},
	}
}

func applyHelmOp(ctx context.Context, helmOps dynamic.ResourceInterface, obj *unstructured.Unstructured) error {
	current, err := helmOps.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		if errorsk8s.IsNotFound(err) {
			_, err = helmOps.Create(ctx, obj, metav1.CreateOptions{})
			return err
		}
		return err
	}
	obj.SetResourceVersion(current.GetResourceVersion())
	_, err = helmOps.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

func InitClusterController(ctx context.Context, mgmtGorizond *controllers.Factory, mgmtManagement *controllersManagement.Factory, mgmtProvision *controllersProvision.Factory, mgmtCore *core.Factory, dynamicClient dynamic.Interface) {
	GorizondResourceController := mgmtGorizond.Provisioning().V1().Cluster()
	SecretResourceController := mgmtCore.Core().V1().Secret()
	NamespaceResourceController := mgmtCore.Core().V1().Namespace()
	ProvisionResourceController := mgmtProvision.Provisioning().V1().Cluster()
	ClusterRoleTemplateBindingController := mgmtManagement.Management().V3().ClusterRoleTemplateBinding()
	UserController := mgmtManagement.Management().V3().User()
	FleetWorkspaceController := mgmtManagement.Management().V3().FleetWorkspace()
	RegistrationTokenResourceController := mgmtManagement.Management().V3().ClusterRegistrationToken()
	helmOps := dynamicClient.Resource(helmOpGVR).Namespace("fleet-default")
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
			k3sLabel := ensureDNSLabel("api-"+uniqName, obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/api")
			headscaleLabel := ensureDNSLabel("headscale-"+uniqName, obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/headscale")
			k3sDomain := k3sLabel + "." + os.Getenv("GORIZOND_DOMAIN_K3S")
			headscaleDomain := headscaleLabel + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE")
			HSServerURL := "http://" + headscaleDomain
			sanitizedNameApi := strings.NewReplacer(
				".", "_",
				"-", "_",
				" ", "_",
				"@", "_",
				"#", "_",
				"$", "_",
			).Replace(obj.Namespace + "_api_" + obj.Status.Cluster)
			helm := map[string]interface{}{
				"repo":        "https://gorizond.github.io/fleet-gorizond-charts/",
				"chart":       "k3s",
				"version":     getK3sChartVersion(),
				"releaseName": normalizeHelmReleaseName(obj.Name+"-k3s", obj.Namespace+"/"+obj.Name+"/k3s"),
				"values": map[string]interface{}{
					"image": map[string]interface{}{
						"k3s": map[string]interface{}{
							"tag": obj.Spec.KubernetesVersion,
						},
					},
					"database":                  strings.ReplaceAll(os.Getenv("DB_DSN_KUBERNETES"), "/gorizond_truncate", "/"+sanitizedNameApi),
					"token":                     obj.Status.HeadscaleToken,
					"headscaleControlServerURL": HSServerURL,
					"ingress": map[string]interface{}{
						"hosts": []interface{}{
							map[string]interface{}{
								"host": k3sDomain,
								"paths": []interface{}{
									map[string]interface{}{
										"path":     "/",
										"pathType": "ImplementationSpecific",
									},
								},
							},
						},
					},
				},
			}
			labels := map[string]string{
				"gorizond-deploy": obj.Name,
				"workspace":       obj.Namespace,
			}
			helmOp := buildHelmOp(normalizeDNSLabel(uniqName+"-k3s", obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/k3s-op"), "fleet-default", obj.Namespace, labels, helm)
			if err := applyHelmOp(ctx, helmOps, helmOp); err != nil {
				return nil, err
			}
			log.Infof("Updated k3s version to %s-->%s for cluster %s/%s", obj.Status.K3sVersion, obj.Spec.KubernetesVersion, obj.Namespace, obj.Name)
			obj.Status.K3sVersion = obj.Spec.KubernetesVersion
			obj.Status.K3sLabel = k3sLabel
			obj.Status.HeadscaleLabel = headscaleLabel
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
			if err := validateProvisionClusterName(obj.Name); err != nil {
				log.Infof("Invalid cluster name %q for %s/%s: %v", obj.Name, obj.Namespace, obj.Name, err)
				if obj.Status.Provisioning != "InvalidName" {
					obj.Status.Provisioning = "InvalidName"
					return GorizondResourceController.Update(obj)
				}
				return obj, nil
			}
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
				return createHeadScaleCreate(ctx, obj, helmOps, GorizondResourceController, sanitizedNameHs)
			}

			if obj.Status.HeadscaleToken == "" && obj.Status.Provisioning == "WaitHeadScaleToken" {
				return createHeadScaleToken(obj, ctx, SecretResourceController, ProvisionResourceController, GorizondResourceController)
			}

			if obj.Status.HeadscaleToken != "" && obj.Status.K3sToken == "" && obj.Status.Provisioning == "WaitKubernetesCreate" {
				return createKubernetesCreate(ctx, obj, helmOps, GorizondResourceController, sanitizedNameApi)
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

		helmOpList, err := helmOps.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return obj, nil
		}
		for _, service := range helmOpList.Items {
			err = helmOps.Delete(ctx, service.GetName(), metav1.DeleteOptions{})
			if err != nil {
				return obj, nil
			}
			log.Infof("deleted helmop %s (%s)", service.GetName(), "fleet-default")
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
		deploymentName := normalizeHelmReleaseName(obj.Name+"-k3s", obj.Namespace+"/"+obj.Name+"/k3s")
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

func createKubernetesCreate(ctx context.Context, obj *gorizondv1.Cluster, helmOps dynamic.ResourceInterface, GorizondResourceController controllersv1.ClusterController, sanitizedNameApi string) (*gorizondv1.Cluster, error) {
	uniqName := obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster
	k3sLabel := ensureDNSLabel("api-"+uniqName, obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/api")
	headscaleLabel := ensureDNSLabel("headscale-"+uniqName, obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/headscale")
	k3sDomain := k3sLabel + "." + os.Getenv("GORIZOND_DOMAIN_K3S")
	headscaleDomain := headscaleLabel + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE")
	HSServerURL := "http://" + headscaleDomain
	helm := map[string]interface{}{
		"repo":        "https://gorizond.github.io/fleet-gorizond-charts/",
		"chart":       "k3s",
		"version":     getK3sChartVersion(),
		"releaseName": normalizeHelmReleaseName(obj.Name+"-k3s", obj.Namespace+"/"+obj.Name+"/k3s"),
		"values": map[string]interface{}{
			"image": map[string]interface{}{
				"k3s": map[string]interface{}{
					"tag": obj.Spec.KubernetesVersion,
				},
			},
			"database":                  strings.ReplaceAll(os.Getenv("DB_DSN_KUBERNETES"), "/gorizond_truncate", "/"+sanitizedNameApi),
			"token":                     obj.Status.HeadscaleToken,
			"headscaleControlServerURL": HSServerURL,
			"ingress": map[string]interface{}{
				"hosts": []interface{}{
					map[string]interface{}{
						"host": k3sDomain,
						"paths": []interface{}{
							map[string]interface{}{
								"path":     "/",
								"pathType": "ImplementationSpecific",
							},
						},
					},
				},
			},
		},
	}
	labels := map[string]string{
		"gorizond-deploy": obj.Name,
		"workspace":       obj.Namespace,
	}
	helmOp := buildHelmOp(normalizeDNSLabel(uniqName+"-k3s", obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/k3s-op"), "fleet-default", obj.Namespace, labels, helm)
	if err := applyHelmOp(ctx, helmOps, helmOp); err != nil {
		return nil, err
	}
	obj.Status.Provisioning = "WaitKubernetesToken"
	obj.Status.K3sVersion = obj.Spec.KubernetesVersion
	obj.Status.K3sLabel = k3sLabel
	obj.Status.HeadscaleLabel = headscaleLabel
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
		deploymentName := normalizeHelmReleaseName(obj.Name+"-headscale", obj.Namespace+"/"+obj.Name+"/headscale")
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

func createHeadScaleCreate(ctx context.Context, obj *gorizondv1.Cluster, helmOps dynamic.ResourceInterface, GorizondResourceController controllersv1.ClusterController, sanitizedNameHs string) (*gorizondv1.Cluster, error) {
	noisePrivateHex, err := generateNoisePrivateHex()
	if err != nil {
		return nil, err
	}
	uniqName := obj.Name + "-" + obj.Namespace + "-" + obj.Status.Cluster
	headscaleLabel := ensureDNSLabel("headscale-"+uniqName, obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/headscale")
	k3sLabel := ensureDNSLabel("api-"+uniqName, obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/api")
	headscaleDomain := headscaleLabel + "." + os.Getenv("GORIZOND_DOMAIN_HEADSCALE")
	domain := headscaleDomain

	dsnHeadScale := dsnparser.Parse(os.Getenv("DB_DSN_HEADSCALE"))
	helm := map[string]interface{}{
		"repo":        "https://gorizond.github.io/fleet-gorizond-charts/",
		"chart":       "headscale",
		"version":     getHeadscaleChartVersion(),
		"releaseName": normalizeHelmReleaseName(obj.Name+"-headscale", obj.Namespace+"/"+obj.Name+"/headscale"),
		"values": map[string]interface{}{
			"database": map[string]interface{}{
				"host": dsnHeadScale.GetHost(),
				"name": sanitizedNameHs,
				"pass": dsnHeadScale.GetPassword(),
				"port": dsnHeadScale.GetPort(),
				"user": dsnHeadScale.GetUser(),
			},
			"ingress": map[string]interface{}{
				"hosts": []interface{}{
					map[string]interface{}{
						"host": domain,
						"paths": []interface{}{
							map[string]interface{}{
								"path":     "/",
								"pathType": "ImplementationSpecific",
							},
						},
					},
				},
			},
			"noise_private": "privkey:" + noisePrivateHex,
		},
	}
	labels := map[string]string{
		"gorizond-deploy": obj.Name,
		"workspace":       obj.Namespace,
	}
	helmOp := buildHelmOp(normalizeDNSLabel(obj.Name+"-"+obj.Namespace+"-"+obj.Status.Cluster+"-headscale", obj.Namespace+"/"+obj.Name+"/"+obj.Status.Cluster+"/headscale-op"), "fleet-default", obj.Namespace, labels, helm)
	if err := applyHelmOp(ctx, helmOps, helmOp); err != nil {
		return nil, err
	}

	obj.Status.Provisioning = "WaitHeadScaleToken"
	obj.Status.HeadscaleLabel = headscaleLabel
	obj.Status.K3sLabel = k3sLabel
	return GorizondResourceController.Update(obj)
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
