package controllers

import (
	"context"
	"fmt"
	"github.com/rancher/lasso/pkg/log"
	"github.com/rancher/wrangler/v3/pkg/data"
	"github.com/rancher/wrangler/v3/pkg/summary"
	controllersProvision "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io"
	cattlev1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/batch"
	coreType "k8s.io/api/core/v1"
	batchv1 "k8s.io/api/batch/v1"
	errorsk8s "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"k8s.io/apimachinery/pkg/api/resource"
)

func getCondition(d data.Object, conditionType string) *summary.Condition {
	for _, cond := range summary.GetUnstructuredConditions(d) {
		if cond.Type() == conditionType {
			return &cond
		}
	}

	return nil
}

func InitReconnectCluster(ctx context.Context, mgmtProvision *controllersProvision.Factory, mgmtBatch *batch.Factory) {
	ProvisionResourceController := mgmtProvision.Provisioning().V1().Cluster()
	BatchResourceController := mgmtBatch.Batch().V1().Job()
	ProvisionResourceController.OnChange(ctx, "gorizond-cluster-reconnect", func(key string, cluster *cattlev1.Cluster) (*cattlev1.Cluster, error) {
		if cluster == nil {
			return nil, nil
		}
			
		d, err := data.Convert(cluster.DeepCopyObject())
		
		if err != nil {
			return nil, err
		}
		
		if cluster.Annotations == nil {
			cluster.Annotations = make(map[string]string)
		}
		
		if cond := getCondition(d, "Ready"); cond != nil && cond.Status() == "False" && cond.Reason() == "Disconnected" {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-helm-disconnected", cluster.Name),
					Namespace: cluster.Namespace,
					Annotations: map[string]string{
						"gorizond-agent-disconnected": "true",
					},
				},
				Spec: batchv1.JobSpec{
					TTLSecondsAfterFinished: pointer.Int32(10),
					Template: coreType.PodTemplateSpec{
						Spec: coreType.PodSpec{
							Containers: []coreType.Container{
								{
									Name:  "helm",
									Image: "alpine/helm",
									Command: []string{"helm", "uninstall", "rancher-webhook", "-n", "cattle-system", "--timeout", "30m", "||", "true"},
									VolumeMounts: []coreType.VolumeMount{
										{
											Name:      "k3s-config",
											MountPath: "/root/.kube",
										},
									},
									Env: []coreType.EnvVar{
										coreType.EnvVar{
											Name: "KUBECONFIG",	
											Value: "/root/.kube/value",
										},
									},
									ImagePullPolicy: coreType.PullIfNotPresent,
									Resources: coreType.ResourceRequirements{
										Requests: coreType.ResourceList{
											coreType.ResourceCPU:    resource.MustParse("10m"),
											coreType.ResourceMemory: resource.MustParse("24Mi"),
										},
									},
								},
							},
							RestartPolicy: coreType.RestartPolicyNever,
							Volumes: []coreType.Volume{
								{
									Name: "k3s-config",
									VolumeSource: coreType.VolumeSource{
										Secret: &coreType.SecretVolumeSource{
											SecretName: cluster.Name + "-kubeconfig",
										},
									},
								},
							},
						},
					},
				},
			}
			_, err = BatchResourceController.Create(job)
			if err != nil && !errorsk8s.IsAlreadyExists(err) {
				log.Infof("Job %s already exists in namespace %s", job.Name, job.Namespace)
			} else {
				return nil, err
			}
			
			cluster.Annotations["gorizond-cluster-connected"] = "false"
			return ProvisionResourceController.Update(cluster)
		}
		return cluster, nil
	})
}
