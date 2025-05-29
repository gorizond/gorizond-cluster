package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterSpec   `json:"spec"`
	Status            ClusterStatus `json:"status,omitempty"`
}

type ClusterSpec struct {
	KubernetesVersion string `json:"kubernetesVersion"`
}

type ClusterStatus struct {
	// +kubebuilder:validation:Format=date-time
    LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	Provisioning   string `json:"provisioning"`
	Cluster        string `json:"cluster"`
	K3sToken       string `json:"k3sToken"`
	K3sVersion     string `json:"k3sVersion"`
	HeadscaleToken string `json:"headscaleToken"`
	Namespace      string `json:"namespace"`
}

// Tag represents a Docker tag
type Tag struct {
	Name string `json:"name"`
}

// GetK3STags returns a list of tags for the rancher/k3s Docker image
func GetK3STags() ([]Tag, error) {
	resp, err := http.Get("https://registry.hub.docker.com/v2/repositories/rancher/k3s/tags/?page_size=100")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Results []Tag `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Results, nil
}

// CheckKubernetesVersion checks if the given Kubernetes version is available and not lower than v1.25.11
func CheckKubernetesVersion(tags []Tag, version string) (string, error) {
	minVersion := "v1.25.11"
	availableVersions := []string{}

	for _, tag := range tags {
		if strings.HasPrefix(tag.Name, "v") {
			availableVersions = append(availableVersions, tag.Name)
		}
	}

	sort.Strings(availableVersions)

	if version < minVersion {
		return "", fmt.Errorf("version %s is lower than minimum required version %s", version, minVersion)
	}

	for _, av := range availableVersions {
		if av == version {
			return version, nil
		}
	}

	return availableVersions[len(availableVersions)-1], fmt.Errorf("version %s is not available, returning latest version %s", version, availableVersions[len(availableVersions)-1])
}

// ValidateKubernetesVersion validates the Kubernetes version in the ClusterSpec
func (c *Cluster) ValidateKubernetesVersion() (string, error) {
	tags, err := GetK3STags()
	if err != nil {
		return "", err
	}

	version := c.Spec.KubernetesVersion
	return CheckKubernetesVersion(tags, version)
}
