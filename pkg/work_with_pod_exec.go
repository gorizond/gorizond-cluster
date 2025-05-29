package pkg

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)
func GetRestConfig(kubeconfigYAML []byte) (*rest.Config, error) {
	config, err := clientcmd.NewClientConfigFromBytes(kubeconfigYAML)
    if err != nil {
        return nil, err
    }
    return config.ClientConfig()
}

func CreateClientset(config *rest.Config) (*kubernetes.Clientset, error) {
    return kubernetes.NewForConfig(config)
}

func GetPodName(clientset *kubernetes.Clientset, ctx context.Context, namespace, deploymentName string, containerName string) (string, error, bool) {
    pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
        LabelSelector: "app.kubernetes.io/instance=" + deploymentName,
    })
    if err != nil {
        return "", err, false
    }
    if len(pods.Items) == 0 {
        return "", fmt.Errorf("no pods found for deployment %s", deploymentName), true
    }
    for _, pod := range pods.Items {
        for _, containerStatus := range pod.Status.ContainerStatuses {
            if containerStatus.Name == containerName && containerStatus.Ready {
                return pod.Name, nil, false
            }
        }
        if len(pod.Status.ContainerStatuses) == 0 {
            return "", fmt.Errorf("no pods status for deployment %s", deploymentName), true
        }
    }
    return "", fmt.Errorf("no pods running for deployment %s", deploymentName), true
}


func ExecCommand(config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string, command []string) (string, string, error) {
    req := clientset.CoreV1().RESTClient().
        Post().
        Resource("pods").
        Name(podName).
        Namespace(namespace).
        SubResource("exec").
        VersionedParams(&corev1.PodExecOptions{
            Command:   command,
            Container: containerName,
            Stdin:     false,
            Stdout:    true,
            Stderr:    true,
            TTY:       false,
        }, scheme.ParameterCodec)

    exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
    if err != nil {
        return "", "", err
    }

    var stdout, stderr bytes.Buffer
    err = exec.Stream(remotecommand.StreamOptions{
        Stdout: &stdout,
        Stderr: &stderr,
    })
    if err != nil {
        return "", "", err
    }

    return stdout.String(), stderr.String(), nil
}
